const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');
const script=fs.readFileSync(path.join(__dirname,'../templates/production_clip_editor.html'),'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
const response=data=>({ok:true,headers:{get:()=> 'application/json'},json:async()=>data});

function picker(talksError=''){
 const elements=new Map(),requests=[];
 function element(){return {disabled:false,value:'',open:false,children:[],style:{},textContent:'',
  replaceChildren(){this.children=[]},appendChild(node){this.children.push(node)},append(...nodes){this.children.push(...nodes)},
  add(node){this.children.push(node);if(!this.value)this.value=node.value},showModal(){this.open=true},addEventListener(){},
  querySelectorAll(){return this.children.flatMap(label=>label.children||[]).filter(node=>node.type==='checkbox'&&node.checked)}
 }}
 const get=id=>{if(!elements.has(id))elements.set(id,element());return elements.get(id)};
 const sandbox={URLSearchParams,Option:function(name,value){this.name=name;this.value=value},previewMode:'edit',conference:'berlin26',settings:{},basename:x=>x,
  document:{getElementById:get,querySelector:()=>({value:'csrf'}),createElement:element,createTextNode:x=>x},
  fetch:async url=>url.includes('template-options')?response({templates:[{id:1,name:'Static A',needsTalk:false},{id:2,name:'Static B',needsTalk:false},{id:0,name:'Talk',needsTalk:true}],talks:[{talk_id:'a',title:'Talk A',eligible:true}],talksError}):new Promise(resolve=>requests.push(resolve))
 };
 vm.createContext(sandbox);vm.runInContext(script,sandbox);
 return {get,requests,open:()=>sandbox.openTemplateImport()};
}

for(const change of ['template','talk selection'])test(`changing ${change} during expansion allows another review and ignores stale results`,async()=>{
 const p=picker();await p.open();const select=p.get('clip-template'),review=p.get('clip-import-preview'),apply=p.get('clip-import-apply');
 if(change==='talk selection'){select.value='0';select.onchange()}
 const first=review.onclick();assert.equal(review.disabled,true);
 if(change==='template'){select.value='2';select.onchange()}
 else{const checkbox=p.get('clip-talks').children[0].children[0];checkbox.checked=true;checkbox.onchange()}
 assert.equal(review.disabled,false);assert.equal(apply.disabled,true);
 const second=review.onclick();
 p.requests[0](response({clips:[{name:'Stale',segments:[]}]}));await first;
 assert.equal(review.disabled,true);assert.equal(apply.disabled,true);
 p.requests[1](response({clips:[{name:'Current',segments:[]}]}));await second;
 assert.equal(review.disabled,false);assert.equal(apply.disabled,false);
 assert.match(p.get('clip-import-review').children[0].children[0].textContent,/Current/);
});

test('talk API failure disables talk templates while static templates can still be reviewed',async()=>{
 const p=picker('Talks unavailable. Retry later.');await p.open();const select=p.get('clip-template'),review=p.get('clip-import-preview');
 select.value='0';select.onchange();assert.equal(review.disabled,true);assert.match(p.get('clip-import-status').textContent,/Talks unavailable/);
 select.value='2';select.onchange();assert.equal(review.disabled,false);assert.equal(p.get('clip-import-status').textContent,'');
 const pending=review.onclick();p.requests[0](response({clips:[{name:'Sponsors',segments:[]}]}));await pending;
 assert.equal(p.get('clip-import-apply').disabled,false);
});
