const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');
const template=fs.readFileSync(path.join(__dirname,'../templates/production_cut.html'),'utf8');
const helpers=template.slice(template.indexOf('let draggedRange='),template.indexOf('function renderRanges()'));

function cutter(activeRange=1){
 const ranges=['a','b','c'].map((name,i)=>({source:`berlin26/${name}.mp4`,sourceType:'video',inMs:i*1000,outMs:i*1000+500,inSet:true,outSet:true}));
 const elements=new Map(),events=[],requests=[];
 const element=id=>{if(!elements.has(id))elements.set(id,{textContent:'',disabled:false});return elements.get(id)};
 const rows=ranges.map(()=>{const classes=new Set();return {classes,classList:{add(...values){values.forEach(v=>classes.add(v))},remove(...values){values.forEach(v=>classes.delete(v))}},contains(){return false}}});
 const sandbox={ranges,activeRange,rangeList:{children:rows},Number,document:{getElementById:element},
  renderRanges(){events.push('render')},syncMarks(){events.push('marks')},
  URLSearchParams,csrf:'csrf',CUTTER:{conference:'berlin26',talks:[{}]},currentTalk:{talk_id:'talk'},currentIndex:0,
  fetch:async(url,options)=>{requests.push({url,body:options.body});return {ok:true}},location:{},talksURL:()=>'/production/timestamp?conference=berlin26'
 };
 vm.createContext(sandbox);vm.runInContext(helpers,sandbox);
 rows.forEach((row,i)=>sandbox.wireRangeDrag(row,i));
 const submit=template.slice(template.indexOf("document.getElementById('cut-form').onsubmit="),template.lastIndexOf('renderTalk();renderRanges();syncMarks();'));
 vm.runInContext(submit,sandbox);
 function event(kind='row'){
  return {target:{closest(selector){return selector==='input'&&kind==='input'||selector==='button'&&['pick','remove'].includes(kind)||selector==='.range-pick'&&kind==='pick'?{}:null}},
   preventDefault(){this.prevented=true},stopPropagation(){},dataTransfer:{setData(){}}};
 }
 return {sandbox,rows,ranges,events,requests,event,submit:()=>element('cut-form').onsubmit({preventDefault(){}})};
}

test('dragging a range preserves selected media and saves timestamps in the new order',async()=>{
 const c=cutter(),selected=c.ranges[1],original=c.ranges.map(range=>({...range}));
 assert.ok(c.rows.every(row=>row.draggable));
 c.rows[1].ondragstart(c.event('pick'));assert.equal(c.rows[1].classes.has('dragging'),true);
 const over=c.event();c.rows[0].ondragover(over);assert.equal(over.prevented,true);assert.equal(c.rows[0].classes.has('drag-over'),true);
 c.rows[0].ondrop(c.event());
 assert.deepEqual(c.ranges,[original[1],original[0],original[2]]);
 assert.equal(c.sandbox.activeRange,0);assert.equal(c.ranges[c.sandbox.activeRange],selected);
 assert.deepEqual(c.events,['render','marks']);assert.ok(c.rows.every(row=>!row.classes.size));
 await c.submit();
 assert.deepEqual(c.requests[0].body.getAll('source'),['berlin26/b.mp4','berlin26/a.mp4','berlin26/c.mp4']);
 assert.deepEqual(c.requests[0].body.getAll('in_ms'),['1000','0','2000']);
 assert.deepEqual(c.requests[0].body.getAll('out_ms'),['1500','500','2500']);
});

test('moving another range across the selection keeps the same active range',()=>{
 const c=cutter(),selected=c.ranges[1];c.sandbox.moveRange(0,2);
 assert.equal(c.ranges[c.sandbox.activeRange],selected);assert.equal(c.sandbox.activeRange,0);
 c.sandbox.moveRange(2,0);assert.equal(c.ranges[c.sandbox.activeRange],selected);assert.equal(c.sandbox.activeRange,1);
});

test('external drops, cancelled drags, same-position drops and invalid moves leave ranges unchanged',()=>{
 const c=cutter(),original=[...c.ranges];
 c.rows[2].ondrop(c.event());c.rows[0].ondragstart(c.event());c.rows[0].ondragend();c.rows[2].ondrop(c.event());
 c.rows[1].ondragstart(c.event());c.rows[1].ondrop(c.event());
 for(const [from,to] of [[-1,0],[0,3],[null,1],[1,NaN]])c.sandbox.moveRange(from,to);
 assert.deepEqual(c.ranges,original);assert.deepEqual(c.events,[]);assert.ok(c.rows.every(row=>!row.classes.size));
});

test('timestamp inputs and other buttons cannot initiate a range drag',()=>{
 const c=cutter();for(const kind of ['input','remove']){const event=c.event(kind);c.rows[0].ondragstart(event);assert.equal(event.prevented,true)}
 c.rows[1].ondrop(c.event());assert.deepEqual(c.events,[]);
});
