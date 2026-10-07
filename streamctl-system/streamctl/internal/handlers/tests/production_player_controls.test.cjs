const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../templates/production_media_preview.html'),'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
const sandbox={};vm.createContext(sandbox);vm.runInContext(source,sandbox);
function target(kind='body'){
 return {closest(selector){return selector.split(',').some(s=>s===kind||s==='button'&&kind==='player-button'||s==='[data-project-preview]'&&kind==='player-button')?{}:null},matches(selector){return selector==='input'&&kind==='input'||selector==='.media-preview-seek'&&kind==='range'}};
}
function harness(){const calls=[];let playing=true;return {calls,player:{playing:()=>playing,togglePlay(){playing=!playing;calls.push(['play',playing])},seekBy(...args){calls.push(['seek',...args])}},mark:which=>calls.push(['mark',which]),key(key,kind='body',extra={}){const event={key,target:target(kind),preventDefault(){this.defaultPrevented=true},...extra};const handled=sandbox.productionPlayerKeydown(event,this.player,this.mark,extra.blocked);return {handled,event}}}}
test('shared transport matches cutter increments and preserves playback',()=>{
 const h=harness();for(const [key,delta] of [['ArrowLeft',-5000],['ArrowRight',5000],[',',-100],['.',100]]){assert.equal(h.key(key).handled,true);assert.deepEqual(h.calls.at(-1),['seek',delta,true])}
 h.key(' ');h.key('.');assert.deepEqual(h.calls.at(-1),['seek',100,false]);h.key('i');h.key('O');assert.deepEqual(h.calls.slice(-2),[['mark','in'],['mark','out']]);
});
test('navigation respects text, dialog, segment, and switch focus and handled events',()=>{
 const h=harness();for(const kind of ['input','select','textarea','button','a','dialog[open]','[contenteditable=true]','.preview-modes','.segment-head'])assert.equal(h.key('ArrowRight',kind).handled,false,kind);
 for(const extra of [{defaultPrevented:true},{blocked:true},{ctrlKey:true},{metaKey:true},{altKey:true}])assert.equal(h.key(' ', 'body',extra).handled,false);
 assert.equal(h.calls.length,0);assert.equal(h.key('Enter').handled,false);
});
test('focused player buttons use native Space once and still support seek shortcuts',()=>{
 const h=harness();assert.equal(h.key(' ','player-button').handled,false);assert.equal(h.key('ArrowRight','player-button').handled,true);assert.equal(h.key('ArrowLeft','range').handled,true);
});
test('one shared control bar is rendered by both engines',()=>{
 const project=fs.readFileSync(path.join(__dirname,'../templates/production_project_preview.html'),'utf8');
 assert.match(project,/template "productionPlayerControls"/);
});
test('timestamp Enter commits and prevents page submission; Escape resets',()=>{
 const calls=[],input={select(){calls.push('select')},blur(){calls.push('blur')}};
 sandbox.bindProductionPlayerTimestamp(input,()=>calls.push('commit'),()=>calls.push('reset'));
 for(const key of ['Enter','Escape'])input.onkeydown({key,preventDefault(){calls.push('prevent')},stopPropagation(){calls.push('stop')}});
 assert.deepEqual(calls,['prevent','stop','commit','blur','prevent','stop','reset','blur']);
});
