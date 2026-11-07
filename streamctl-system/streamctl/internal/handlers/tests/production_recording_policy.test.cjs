const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

test('cutter navigation updates visible recording preference and safely displays speaker names',()=>{
 const elements=new Map();
 const element=()=>({textContent:'',className:'',children:[],replaceChildren(){this.children=[]},append(...nodes){this.children.push(...nodes)}});
 const document={createElement:element,getElementById(id){if(!elements.has(id))elements.set(id,element());return elements.get(id)}};
 const shared=fs.readFileSync(path.join(__dirname,'../templates/production_recording_policy.html'),'utf8').match(/<script>([\s\S]*?)<\/script>/)[1].replace('{{recordingPolicyCatalog}}','{}');
 const cutter=fs.readFileSync(path.join(__dirname,'../templates/production_cut.html'),'utf8');
 const renderTalk=cutter.slice(cutter.indexOf('function renderTalk()'),cutter.indexOf('function syncMarks()'));
 const talks=[
  {title:'Allowed',recordingNotice:{level:'allowed'}},
  {title:'Slides',recordingNotice:{title:'<Ada>',label:'Slides + audio only — no speaker video',detail:'Exclude speaker camera footage.',level:'limited'}},
  {title:'Private',recordingNotice:{title:'Grace',label:'Do not record',detail:'Exclude this talk.',level:'prohibited'}},
  {title:'Allowed again',recordingNotice:{level:'allowed'}},
  {title:'Unknown',recordingNotice:{title:'Lin',label:'Recording preference unverified',detail:'Verify consent.',level:'unknown'}}
 ];
 const sandbox={document,CUTTER:{talks},currentIndex:0,currentTalk:talks[0]};
 vm.createContext(sandbox);vm.runInContext(shared+renderTalk,sandbox);
 for(let i=0;i<talks.length;i++){
  sandbox.currentIndex=i;sandbox.currentTalk=talks[i];sandbox.renderTalk();
  const notice=document.getElementById('cutter-recording-policy');
  const restricted=['limited','prohibited'].includes(talks[i].recordingNotice.level);
  assert.equal(notice.hidden,!restricted);
  if(!restricted){assert.equal(notice.children.length,0);continue}
  assert.equal(notice.className,'recording-policy recording-policy-'+talks[i].recordingNotice.level);
  assert.equal(notice.children[0].textContent,talks[i].recordingNotice.title+' · '+talks[i].recordingNotice.label);
  assert.equal(notice.children.length,2);
 }
});
