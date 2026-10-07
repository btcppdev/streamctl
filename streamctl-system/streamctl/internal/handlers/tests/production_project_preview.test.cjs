const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');
const shared=fs.readFileSync(path.join(__dirname,'../templates/production_media_preview.html'),'utf8').match(/<script>([\s\S]*?)<\/script>/)[1].split('function createProductionMediaPreview')[0];
const script=shared+fs.readFileSync(path.join(__dirname,'../templates/production_project_preview.html'),'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
const pure={};vm.createContext(pure);vm.runInContext(script+';this.timeline=ProductionProjectTimeline;',pure);const T=pure.timeline;
const image=(src='card.png',durationMs=4000)=>({type:'image',src,durationMs});
const video=(src='main.mp4',extra={})=>({type:'video',src,...extra});
const info=(durationMs=10000)=>({durationMs,proxyPath:'berlin26/workspace/proxy.mp4'});
const plan=(segments,settings={})=>T.build(segments,settings,new Map(segments.filter(s=>s.type!=='image').map(s=>[s.src,info()])));

test('timeline matches the pinned conf-render overlap fixture',()=>{
 const p=plan([image('a.png',3000),video('b.mp4',{in:'00:00:01',out:'00:00:05'})]);
 assert.equal(p.entries[1].start,2000);assert.equal(p.duration,6000);assert.equal(p.entries[1].sourceIn,1000);
});
test('image defaults, chunked proxy durations, and segment-relative audio offsets',()=>{
 const p=plan([{type:'image',src:'a.png'},video('b.mp4',{type:'chunkedVideo',in:'00:00:02',audio:{src:'music.mp3',in:'00:00:03',mode:'mix',gainDb:-6,sourceGainDb:-3}})],{imageMs:5000});
 assert.equal(p.entries[0].duration,5000);assert.equal(p.entries[1].duration,8000);assert.equal(p.entries[1].audio.inMs,3000);
});
test('linear dissolve weights, incoming tie selection, and endpoint fades',()=>{
 const p=plan([image('a.png',3000),image('b.png',4000)]),s=T.state(p,2500);
 assert.equal(s.weights[0],.5);assert.equal(s.weights[1],.5);assert.equal(s.dominant.index,1);
 assert.equal(T.state(p,500).fade,.5);assert.equal(T.state(p,p.duration-500).fade,.5);
 assert.equal(T.state(p,0).fade,0);assert.equal(T.state(p,p.duration).fade,0);
 assert.equal(T.selection(p,2500).sourceMs,500);
});
test('multiple simultaneous overlaps preserve ordered crossfade weights',()=>{
 const p=plan([image('a.png',1000),image('b.png',1000),image('c.png',1000)]),s=T.state(p,500);
 assert.deepEqual(Array.from(s.weights),[.25,.25,.5]);assert.equal(s.active.length,3);assert.equal(s.dominant.index,2);
});
test('mapping clamps source positions and retains transition/image project positions',()=>{
 const p=plan([image('a.png',3000),video('b.mp4',{in:'00:00:01',out:'00:00:05'})]);
 assert.equal(T.fromSource(p,1,0),2000);assert.equal(T.fromSource(p,1,2000),3000);assert.equal(T.fromSource(p,1,99999),6000);
 assert.equal(T.selection(p,1234).sourceMs,1234);
 const selected=T.selection(p,2500);assert.equal(T.fromSource(p,selected.index,selected.sourceMs),2500);
});
test('invalid cuts, durations, audio settings, and unprepared sources identify the segment',()=>{
 for(const s of [video('main.mp4',{in:'invalid'}),video('main.mp4',{out:'00:00:11'}),video('main.mp4',{in:'00:00:02',out:'00:00:01'}),video('main.mp4',{audio:{src:''}}),{type:'streamctl.talkCuts'}])assert.throws(()=>plan([s]),/Segment 1:/);
 assert.throws(()=>plan([image('a.png',500),image()]),/at least the transition/);
 assert.throws(()=>T.build([video()],{},new Map()),/Segment 1:.*Prepare/);
});

function player(fetchInfo=async()=>info()){
 const frames=new Map(),media=[],images=[],gains=[],contexts=[];let frameId=0;
 class Element{
  constructor(tag='div'){this.tag=tag;this.children=new Map();this.nodes=[];this.style={};this.dataset={};this.attributes={};this.textContent='';this.value='';this.disabled=false;this.hidden=false;this.alive=true}
  querySelector(s){if(!this.children.has(s))this.children.set(s,new Element());return this.children.get(s)}
  appendChild(n){n.parent?.removeChild(n);this.nodes.push(n);n.parent=this;return n}
  removeChild(n){this.nodes=this.nodes.filter(x=>x!==n)}
  remove(){this.parent?.removeChild(this);this.alive=false}
  removeAttribute(k){delete this.attributes[k]}
  setAttribute(k,v){this.attributes[k]=v}
  getBoundingClientRect(){return {width:0,height:58}}
  setPointerCapture(){} focus(){} blur(){}
  getContext(){return {setTransform(){},clearRect(){},beginPath(){},moveTo(){},lineTo(){},stroke(){},fillText(){}}}
 }
 class Media extends Element{
  constructor(tag){super(tag);this.readyState=0;this.paused=true;this.clock=0;this.seeking=false;this.duration=300;this.seeks=[]}
  get currentTime(){return this.clock}
  set currentTime(t){this.clock=t;this.seeks.push(t);this.seeking=true}
  get src(){return this.attributes.src}set src(v){this.attributes.src=v}
  load(){this.readyState=0;this.seeking=false;this.clock=0}
  pause(){this.paused=true}
  play(){this.paused=false;return Promise.resolve()}
  metadata(){this.readyState=1;this.onloadedmetadata()}
  settled(t=this.clock){this.clock=t;this.seeking=false;this.readyState=4;this.onseeked()}
  canplay(){this.readyState=4;this.oncanplay()}
 }
 class AudioContext{
  constructor(){this.currentTime=0;this.destination={};contexts.push(this)}
  resume(){return Promise.resolve()}
  createMediaElementSource(node){return {node,connect(){},disconnect(){}}}
  createGain(){const node={gain:{value:0,cancelScheduledValues(){},setValueAtTime(v){this.value=v},linearRampToValueAtTime(v){this.future=v}},connect(){},disconnect(){}};gains.push(node);return node}
 }
 const root=new Element(),sandbox={URLSearchParams,AbortController,structuredClone,getComputedStyle(){return {getPropertyValue(){return ''}}},window:{AudioContext,devicePixelRatio:1},document:{activeElement:null,createElement(tag){if(['video','audio'].includes(tag)){const n=new Media(tag);media.push(n);return n}const n=new Element(tag);if(tag==='img')images.push(n);return n}},requestAnimationFrame(fn){frames.set(++frameId,fn);return frameId},cancelAnimationFrame(id){frames.delete(id)}};
 vm.createContext(sandbox);vm.runInContext(script,sandbox);const preview=sandbox.createProductionProjectPreview(root,{fetchInfo});
 return {preview,root,media,images,gains,contexts,
  async open(segments,settings={},at={}){return preview.open(segments,settings,at)},
  ready(){for(let cycle=0;cycle<30;cycle++){let changed=false;for(const n of media){if(!n.parent?.alive)continue;if(n.readyState===0){changed=true;n.metadata()}if(n.seeking){changed=true;n.settled()}if(n.readyState===1){changed=true;n.canplay()}}for(const n of images)if(n.parent?.alive&&!n.projectReady){changed=true;n.projectReady=true;n.onload()}if(!changed)return}throw Error('media never settled')},
  tick(ms){for(const c of contexts)c.currentTime+=ms/1000;for(const n of media)if(!n.paused)n.clock+=ms/1000;const pending=[...frames.values()];frames.clear();pending.forEach(fn=>fn())}
 };
}
test('project metadata is deduplicated and concurrency stays at three',async()=>{
 let active=0,max=0,calls=0;const releases=[];
 const p=player(async()=>{calls++;active++;max=Math.max(max,active);await new Promise(resolve=>releases.push(resolve));active--;return info()});
 const opened=p.open(['a','b','c','d','a'].map(x=>video(x)));await Promise.resolve();assert.equal(calls,3);
 releases.splice(0).forEach(fn=>fn());await new Promise(resolve=>setImmediate(resolve));assert.equal(calls,4);
 releases.splice(0).forEach(fn=>fn());await opened;assert.equal(max,3);
});
test('cards advance on the project clock and hand off inside their duration',async()=>{
 const p=player();await p.open([image('a.png',3000),image('b.png',4000)]);p.ready();await p.preview.play();p.tick(2500);p.ready();
 assert.equal(p.preview.selection().index,1);assert.equal(p.preview.selection().sourceMs,500);assert.equal(p.preview.position(),2500);
 p.preview.pause();p.tick(1000);assert.equal(p.preview.position(),2500);
});
test('same-source adjacent cuts use separate elements for the transition',async()=>{
 const p=player();await p.open([video('a.mp4',{in:'00:00:01',out:'00:00:04'}),video('a.mp4',{in:'00:00:06',out:'00:00:09'})],{}, {projectMs:2500});p.ready();
 const videos=p.media.filter(n=>n.tag==='video');assert.equal(videos.length,2);assert.equal(videos[0].currentTime,3.5);assert.equal(videos[1].currentTime,6.5);
});
test('rapid seeks coalesce while the media seek is outstanding',async()=>{
 const p=player();await p.open([video()]);p.ready();const node=p.media[0];p.preview.seek(2000);p.preview.seek(4000);p.preview.seek(6000);
 assert.deepEqual(node.seeks,[2]);node.settled();assert.deepEqual(node.seeks,[2,6]);node.settled();assert.equal(p.preview.position(),6000);
});
test('mix and replace gains include segment crossfades and audio offsets',async()=>{
 const p=player();await p.open([video('a.mp4',{out:'00:00:03',audio:{src:'music.mp3',mode:'replace',in:'00:00:20',gainDb:-6}}),video('b.mp4',{out:'00:00:04',audio:{src:'music.mp3',mode:'mix',sourceGainDb:-3,gainDb:0}})],{}, {projectMs:2500});p.ready();await p.preview.play();
 assert.equal(p.media[1].currentTime,22.5);assert.equal(p.gains[0].gain.value,0);assert.ok(Math.abs(p.gains[1].gain.value-.5*Math.pow(10,-6/20))<1e-9);
 assert.ok(Math.abs(p.gains[2].gain.value-.5*Math.pow(10,-3/20))<1e-9);assert.equal(p.gains[3].gain.value,.5);
});
test('image external audio plays, and ends with silence padding',async()=>{
 const p=player();await p.open([{...image('a.png',4000),audio:{src:'music.mp3',in:'00:00:01'}}]);p.ready();p.media[0].duration=2;
 await p.preview.play();assert.equal(p.media[0].paused,false);p.tick(1500);assert.equal(p.media[0].paused,true);assert.equal(p.preview.playing(),true);
});
test('simultaneous buffering freezes the project clock until all tracks are ready',async()=>{
 const p=player();await p.open([video('a.mp4',{audio:{src:'music.mp3'}})]);p.ready();await p.preview.play();p.tick(1500);
 for(const n of p.media){n.readyState=1;n.onwaiting()}p.tick(1000);assert.equal(p.preview.position(),1500);
 p.media[0].canplay();assert.equal(p.media[0].paused,true);p.media[1].canplay();assert.equal(p.media[0].paused,false);assert.equal(p.media[1].paused,false);
 p.tick(1000);assert.equal(p.preview.position(),2500);
});
test('pool stays bounded to active and next segments on distant seeks',async()=>{
 const p=player();await p.open(Array.from({length:20},(_,i)=>video(`v${i}.mp4`)));p.ready();p.preview.seek(100000);p.ready();
 assert.ok(p.media.filter(n=>n.parent?.alive).length<=3);assert.equal(p.preview.selection().index,11);
});
test('stale load events and metadata completions are ignored after close',async()=>{
 let resolve;const p=player(()=>new Promise(r=>resolve=r));const opened=p.open([video()]);p.preview.close();resolve(info());assert.equal(await opened,false);assert.equal(p.media.length,0);
 const q=player();await q.open([video()]);const old=q.media[0];q.preview.close();old.onerror();assert.equal(q.root.querySelector('.media-preview-status').textContent,'Loading project position…');
});
test('asset errors and invalid external audio in are explicit and stop playback',async()=>{
 const p=player();await p.open([image()]);p.images[0].onerror();assert.match(p.root.querySelector('.media-preview-status').textContent,/Segment 1:.*image/);assert.equal(p.preview.playing(),false);
 const q=player();await q.open([video('a.mp4',{audio:{src:'music.mp3',in:'00:06:00'}})]);q.ready();assert.match(q.root.querySelector('.media-preview-status').textContent,/Audio in is outside/);
});
test('end of project pauses everything and replay starts from zero',async()=>{
 const p=player();await p.open([video('a.mp4',{out:'00:00:02'})]);p.ready();await p.preview.play();p.tick(2000);assert.equal(p.preview.playing(),false);assert.equal(p.preview.position(),2000);
 await p.preview.play();p.ready();assert.equal(p.preview.position(),0);assert.equal(p.preview.playing(),true);
});
test('fine dragging stays paused, coarse dragging resumes only prior playback',async()=>{
 const p=player();await p.open([video()]);p.ready();await p.preview.play();const fine=p.root.querySelector('.media-preview-fine'),coarse=p.root.querySelector('.media-preview-seek'),event={pointerId:1,clientX:100,button:0,preventDefault(){}};
 fine.getBoundingClientRect=()=>({width:1000,height:58});fine.onpointerdown(event);fine.onpointermove({...event,clientX:0});fine.onpointerup(event);p.ready();assert.equal(p.preview.playing(),false);
 await p.preview.play();coarse.onpointerdown(event);coarse.value=2000;coarse.oninput();coarse.onpointerup(event);p.ready();assert.equal(p.preview.playing(),true);
});

test('short projects multiply overlapping opening and closing fades like FFmpeg',()=>{
 const p=plan([image('a.png',500)]);assert.equal(T.state(p,250).fade,.25);
});
function editor(segments){
 const controls=[{disabled:false,closest(){return false}},{disabled:true,closest(){return false}},{disabled:false,closest(){return true}}];
 function classes(){const values=new Set();return {toggle(){},add(v){values.add(v)},remove(v){values.delete(v)},contains(v){return values.has(v)}}}
 const cards=segments.map(()=>({draggable:true,classList:classes(),querySelector(){return {setAttribute(){}}}}));const roots=new Map();const element=key=>{if(!roots.has(key))roots.set(key,{hidden:false,setAttribute(){}});return roots.get(key)};
 let sourceMs=0,editPlaying=false,projectPosition=0,projectPlan=null;const opens=[];
 const preview={currentTime:()=>sourceMs,ready:()=>true,playing:()=>editPlaying,togglePlay:()=>editPlaying=false};
 const project={async open(segments,settings,at){projectPlan=plan(segments,settings);projectPosition=Number.isFinite(at.projectMs)?at.projectMs:T.fromSource(projectPlan,at.index,at.sourceMs);opens.push(at)},pause(){},close(){},plan:()=>projectPlan,seek(ms){projectPosition=ms},selection(){return projectPlan?T.selection(projectPlan,projectPosition):null}};
 const sandbox={editorKind:'render',selectedIndex:0,segments,settings:{},preview,segmentList:{children:cards,classList:classes()},document:{querySelector:element,getElementById:element,querySelectorAll(selector){return selector==='.segment-card'?cards:controls}},createProductionProjectPreview:()=>project,previewSelected(ms){sourceMs=ms??0}};
 vm.createContext(sandbox);const template=fs.readFileSync(path.join(__dirname,'../templates/production_template_edit.html'),'utf8');const helper=template.slice(template.indexOf("let previewMode='edit'"),template.indexOf('function basename'));
 vm.runInContext(helper,sandbox);
 return {sandbox,controls,cards,opens,setSource(ms){sourceMs=ms},setProject(ms){projectPosition=ms},setPlaying(){editPlaying=true},isEditPlaying:()=>editPlaying,mode:mode=>sandbox.setPreviewMode(mode),source:()=>sourceMs};
}
test('project mode locks editing and dragging, then restores prior disabled state',async()=>{
 const e=editor([video()]);e.setPlaying();await e.mode('project');assert.equal(e.isEditPlaying(),false);assert.deepEqual(e.controls.map(c=>c.disabled),[true,true,false]);assert.equal(e.cards[0].draggable,false);
 await e.mode('edit');assert.deepEqual(e.controls.map(c=>c.disabled),[false,true,false]);assert.equal(e.cards[0].draggable,true);
});
test('mode handoff preserves the exact point inside an image and a dissolve',async()=>{
 const e=editor([image('a.png',3000),video('b.mp4',{in:'00:00:01',out:'00:00:05'})]);
 await e.mode('project');e.setProject(1234);await e.mode('edit');e.setSource(0);await e.mode('project');assert.equal(e.opens.at(-1).projectMs,1234);
 e.setProject(2500);await e.mode('edit');assert.equal(e.sandbox.selectedIndex,1);assert.equal(e.source(),1500);
 await e.mode('project');assert.equal(e.opens.at(-1).projectMs,2500);
});
test('edits and source scrubbing discard the old bookmark and rebuild from Edit',async()=>{
 const e=editor([video()]);await e.mode('project');e.setProject(3000);await e.mode('edit');e.setSource(5000);await e.mode('project');assert.equal(e.opens.at(-1).sourceMs,5000);
 await e.mode('edit');e.sandbox.segments[0].in='00:00:01';await e.mode('project');assert.equal(e.opens.at(-1).projectMs,undefined);
});

test('reopening during metadata loading does not reuse an aborted request',async()=>{
 const requests=[];const p=player((key,signal)=>new Promise((resolve,reject)=>{requests.push({resolve,reject});signal.addEventListener('abort',()=>reject(Error('aborted')))}));
 const first=p.open([video()]);const second=p.open([video()]);assert.equal(requests.length,2);requests[1].resolve(info());
 assert.equal(await first,false);assert.equal(await second,true);p.ready();assert.equal(p.preview.ready(),true);
});
test('ended media does not restart at zero just before its project boundary',async()=>{
 const p=player();await p.open([video()]);p.ready();await p.preview.play();p.tick(9950);p.media[0].ended=true;p.media[0].paused=true;p.tick(10);assert.equal(p.media[0].paused,true);
});

test('fine paused seeks request small movements and accept rounded media clocks',async()=>{
 const p=player();await p.open([video()]);p.ready();p.preview.seek(20);assert.equal(p.media[0].seeks.at(-1),.02);p.media[0].settled(0);assert.equal(p.preview.ready(),true);assert.equal(p.media[0].seeks.length,1);
 p.preview.seek(40);assert.equal(p.media[0].seeks.at(-1),.04);p.media[0].settled();assert.equal(p.preview.position(),40);
});

test('preview collapses all rows, restores expanded rows, and pins a paused list jump',async()=>{
 const e=editor([image('a.png',3000),video('b.mp4',{in:'00:00:01',out:'00:00:05'})]);
 e.cards[0].classList.add('open');e.cards[1].classList.add('open');await e.mode('project');assert.equal(e.cards.some(c=>c.classList.contains('open')),false);
 e.sandbox.selectProjectSegment(1);await e.mode('edit');assert.equal(e.sandbox.selectedIndex,1);assert.equal(e.source(),1000);assert.equal(e.cards.every(c=>c.classList.contains('open')),true);
 await e.mode('project');assert.equal(e.opens.at(-1).projectMs,2000);
});
test('project label uses imported title after source and trim changes',async()=>{
 const p=player();await p.open([video('edited.mp4',{in:'00:00:01',streamctl:{talkTitle:'Original imported talk',talkSchedule:'Oct 1 · 11:30–12:00 CEST'}})]);p.ready();assert.match(p.root.querySelector('.media-preview-source-name').textContent,/Original imported talk/);assert.match(p.root.querySelector('.media-preview-source-name').textContent,/11:30–12:00 CEST/);
});

test('timestamp Enter commits directly to the project engine while staying paused',async()=>{
 const p=player();await p.open([video()]);p.ready();const current=p.root.querySelector('.media-preview-current');current.value='00:00:04.500';
 current.onkeydown({key:'Enter',preventDefault(){},stopPropagation(){}});p.ready();assert.equal(p.preview.position(),4500);assert.equal(p.preview.playing(),false);
});
