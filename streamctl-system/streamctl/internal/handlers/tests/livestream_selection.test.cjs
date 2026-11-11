const {test}=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const html=fs.readFileSync(path.join(__dirname,'../templates/production_renders.html'),'utf8');
const script=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m=>m[1]).find(s=>s.includes('function livestreamTalkGroups'));
function fixture(){
 const elements=new Map();
 class Element{
  constructor(){this.children=[];this.style={};this.disabled=false;this.checked=false;this.open=false;this.indeterminate=false;this.value='';this.listeners={}}
  append(...nodes){this.children.push(...nodes)} prepend(...nodes){this.children.unshift(...nodes)} replaceChildren(){this.children=[]}
  setAttribute(k,v){this[k]=v} addEventListener(k,fn){this.listeners[k]=fn} showModal(){this.open=true} close(){this.open=false;this.listeners.close?.()} focus(){}
  querySelector(){return new Element()}
  querySelectorAll(selector){const all=this.children.flatMap(x=>x instanceof Element?[x,...x.querySelectorAll('*')]:[]);return selector==='*'?all:all.filter(n=>n.name==='talk_ids')}
 }
 const get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id)};get('livestream-form').elements={template_id:new Element(),conference:{value:'seoul'}};
 const talk=(id,day,stage,ready=true)=>({talk_id:id,title:id,starts_at:`2026-11-0${day}T10:00:00+09:00`,dayLabel:'Day '+(day-4),dateLabel:'Nov '+day,stageLabel:stage,ready,timeLabel:'10:00',reason:ready?'':'No cuts'});
 const talks=[talk('a',5,'Main'),talk('b',5,'Main'),talk('uncut',5,'Main',false),talk('c',5,'Talks'),talk('d',6,'Main'),{...talk('restricted',5,'Main'),manualOnly:true}];
 const ctx={Map,Set,URLSearchParams,renderCreateMenu:{},document:{getElementById:get,createElement:()=>new Element(),createTextNode:x=>x},fetch:async()=>({ok:true,headers:{get:()=> 'application/json'},json:async()=>({talks})})};
 vm.createContext(ctx);vm.runInContext(script,ctx);
 return {ctx,get,checks:()=>get('livestream-talks').querySelectorAll('*'),open:async()=>{await get('render-livestream').onclick()}};
}
test('talks are grouped by day and stage, with whole-group and individual selection',async()=>{
 const f=fixture();await f.open();const checks=f.checks();const group=checks.find(c=>c['aria-label']==='Select Day 1 · Main'),a=checks.find(c=>c.value==='a'),b=checks.find(c=>c.value==='b'),uncut=checks.find(c=>c.value==='uncut');
 assert.equal(f.get('livestream-create').disabled,true);group.checked=true;group.onchange();assert.equal(a.checked,true);assert.equal(b.checked,true);assert.equal(uncut.checked,false);assert.equal(group.indeterminate,false);assert.equal(checks.find(c=>c.value==='restricted').checked,false);assert.match(f.get('livestream-create').textContent,/2 talks/);
 a.checked=false;a.onchange();assert.equal(group.indeterminate,true);assert.equal(group.checked,false);
 const combo=checks.find(c=>c['aria-label']==='Select Day 1 · Talks');combo.checked=true;combo.onchange();assert.equal(checks.find(c=>c.value==='c').checked,true);assert.equal(checks.find(c=>c.value==='d').checked,false);assert.match(f.get('livestream-create').textContent,/2 talks/);
 assert.equal(checks.filter(c=>c['aria-label']?.startsWith('Select ')).length,3);
 assert.equal(checks.some(c=>c['aria-label']==='Select Day 1 · Nov 5'),false);
 assert.equal(f.get('livestream-talks').children.length,3);
 f.get('livestream-select-none').onclick();assert.equal(f.get('livestream-create').disabled,true);
 f.get('livestream-select-all').onclick();assert.match(f.get('livestream-create').textContent,/4 talks/);
 const restricted=checks.find(c=>c.value==='restricted');assert.equal(restricted.disabled,false);assert.equal(restricted.checked,false);restricted.checked=true;restricted.onchange();assert.match(f.get('livestream-create').textContent,/5 talks/);
});
test('new dialog no longer contains day or stage dropdowns',()=>{
 assert.doesNotMatch(html,/id="livestream-(day|stage)"/);
});
