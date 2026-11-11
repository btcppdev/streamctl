const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const script=fs.readFileSync(path.join(__dirname,'../templates/live_updates.html'),'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
function fixture(){
 let next=0;const timers=new Map(),listeners={},pending=[];
 const document={hidden:false,activeElement:null,addEventListener:(event,fn)=>listeners[event]=fn,removeEventListener:event=>delete listeners[event]};
 const ctx={document,window:{addEventListener:(event,fn)=>listeners[event]=fn},AbortController,Date,Set,
 setTimeout:(fn,ms)=>{const id=++next;timers.set(id,{fn,ms});return id},clearTimeout:id=>timers.delete(id),fetch:()=>new Promise((resolve,reject)=>pending.push({resolve,reject}))};
 vm.createContext(ctx);vm.runInContext(script,ctx);return {ctx,timers,listeners,pending,document};
}
test('live updates do not overlap, back off on errors, and recover without navigation',async()=>{
 const f=fixture(),applied=[],status={textContent:''};const live=f.ctx.createLiveUpdates({url:'/worker/jobs/status',apply:s=>applied.push(s),status});
 const first=live.update();await live.update();assert.equal(f.pending.length,1);
 f.pending[0].resolve({ok:false,redirected:false});await first;assert.match(status.textContent,/stale/);assert.ok([...f.timers.values()].some(t=>t.ms===10000));
 const second=live.update();f.pending[1].resolve({ok:true,redirected:false,text:async()=>'<p>finished</p>'});await second;assert.deepEqual(applied,['<p>finished</p>']);assert.ok([...f.timers.values()].some(t=>t.ms===5000));
});
test('hidden pages do not poll and late results after leaving are ignored',async()=>{
 const f=fixture(),applied=[];const live=f.ctx.createLiveUpdates({url:'/jobs',apply:x=>applied.push(x)});
 f.document.hidden=true;await live.update();assert.equal(f.pending.length,0);
 f.document.hidden=false;const work=live.update();live.stop();f.pending[0].resolve({ok:true,redirected:false,text:async()=>'stale'});await work;assert.equal(applied.length,0);assert.equal(f.timers.size,0);
});
test('authentication redirects are errors, never inserted as page content',async()=>{
 const f=fixture(),applied=[];const live=f.ctx.createLiveUpdates({url:'/jobs',apply:x=>applied.push(x)});const work=live.update();f.pending[0].resolve({ok:true,redirected:true});await work;assert.equal(applied.length,0);
});
test('job updates preserve focused actions and expanded errors',()=>{
 const f=fixture(),details=[{dataset:{jobError:'render-1'},open:false}],root={innerHTML:'old',contains:x=>x==='button',querySelectorAll:s=>s==='select[name]'?[]:s==='details[open]'?[{dataset:{jobError:'render-1'}}]:details};
 f.document.activeElement='button';f.ctx.updateJobList(root,'new');assert.equal(root.innerHTML,'old');
 f.document.activeElement=null;f.ctx.updateJobList(root,'new');assert.equal(root.innerHTML,'new');assert.equal(details[0].open,true);
});
