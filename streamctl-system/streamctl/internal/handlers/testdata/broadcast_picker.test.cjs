// Run with: node --test internal/handlers/testdata/broadcast_picker.test.cjs
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(require('node:path').join(__dirname, '../templates/broadcast_picker.html'), 'utf8')
    .split('<script>')[1].split('</script>')[0]
    .replace("{{.BroadcastMode}}", "null")
    .replace("{{if and .Stream .Stream.ID}}'none'{{else}}'auto'{{end}}", "'auto'");
const conf = {tag:'toronto', description:'Toronto', starts_at:'2026-07-22', talks:[
    {title:'Talk One', speakers:[{name:'Alice'}], recording:{id:'one',file_uri:'toronto/recordings/edits/one.mp4'}},
    {title:'No recording', speakers:[], recording:null},
]};
async function picker({catalog=[conf], clips=['toronto/recordings/edits/one.mp4'], saved='', fail=false}={}) {
    const elements = new Map();
    function element(id) {
        if (!elements.has(id)) elements.set(id, {value:id==='broadcast-recording'?saved:'', hidden:false, children:[], listeners:{},
            addEventListener(name, fn){this.listeners[name]=fn;},
            closest(){return element("form");},
            replaceChildren(){this.children=[]; this.value='';},
            add(opt){this.children.push(opt); if(opt.selected)this.value=opt.value;}});
        return elements.get(id);
    }
    const window={};
    const state={clips};
    vm.runInNewContext(source, {window,document:{getElementById:element,querySelectorAll:()=>state.clips.map(value=>({value}))},
        Option:function(text,value,_,selected){Object.assign(this,{text,value,selected});},
        fetch:async()=>({ok:!fail,json:async()=>catalog,text:async()=> 'unavailable'})});
    await new Promise(resolve=>setImmediate(resolve));
    return {element,state,window,change(id,value){const el=element(id);el.value=value;el.listeners.change();}};
}
test('unique full-path match becomes the default recording and follows clip changes',async()=>{
    const p=await picker();
    assert.equal(p.element('broadcast-recording').value,'one');
    p.state.clips=['elsewhere/one.mp4']; p.window.refreshBroadcastMatch();
    assert.equal(p.element('broadcast-recording').value,'');
});
test('ambiguous recordings and playlists are not auto-linked',async()=>{
    for(const options of [
        {clips:['toronto/recordings/edits/one.mp4','second.mp4']},
        {catalog:[conf,{...conf,tag:'other',talks:[{...conf.talks[0],recording:{...conf.talks[0].recording,id:'two'}}]}]},
        {clips:['one.mp4']}
    ]) assert.equal((await picker(options)).element('broadcast-recording').value,'');
});
test('explicit conference selection survives clip changes and excludes recording target',async()=>{
    const p=await picker(); p.change('broadcast-mode','conference');
    p.change('conference-choice','toronto');
    p.state.clips=['different.mp4']; p.window.refreshBroadcastMatch();
    assert.equal(p.element('broadcast-conference').value,'toronto');
    assert.equal(p.element('broadcast-recording').value,'');
});
test('saved links survive API failures and do not get auto-replaced',async()=>{
    for(const fail of [false,true]) {
        const p=await picker({saved:'original',fail});
        assert.equal(p.element('broadcast-recording').value,'original');
    }
});
test('search filters talks by speaker and unrecorded talks cannot be selected',async()=>{
    const p=await picker({clips:[]});p.change('broadcast-mode','talk');p.change('conference-choice','toronto');
    assert.equal(p.element('talk-choice').children.find(o=>o.text.includes('No recording')).disabled,true);
    p.element('talk-search').value='alice';p.element('talk-search').listeners.input();
    assert.deepEqual(p.element('talk-choice').children.map(o=>o.value),['','one']);
    p.change('talk-choice','one');
    assert.equal(p.element('broadcast-recording').value,'one');
    assert.equal(p.element('broadcast-conference').value,'');
    p.change('broadcast-mode','none');
    assert.equal(p.element('broadcast-recording').value,'');
});

test('saving waits for auto lookup or a manual choice when the API is unavailable',async()=>{
    const p=await picker({fail:true});
    let prevented=false;
    p.element('form').listeners.submit({preventDefault(){prevented=true;}});
    assert.equal(prevented,true);
    p.change('broadcast-mode','none');prevented=false;
    p.element('form').listeners.submit({preventDefault(){prevented=true;}});
    assert.equal(prevented,false);
});

test('automatic matches remain visible and can be overridden directly', async()=>{
    const alternate={title:'Talk Two',speakers:[],recording:{id:'two',file_uri:'other.mp4'}};
    const p=await picker({catalog:[{...conf,talks:[...conf.talks,alternate]}]});
    assert.equal(p.element('broadcast-choices').hidden,false);
    assert.equal(p.element('talk-choices').hidden,false);
    assert.equal(p.element('conference-choice').value,'toronto');
    assert.equal(p.element('talk-choice').value,'one');
    p.change('talk-choice','two');
    assert.equal(p.element('broadcast-mode').value,'talk');
    p.window.refreshBroadcastMatch();
    assert.equal(p.element('broadcast-recording').value,'two');
});
test('changing the conference directly leaves automatic matching and clears the old talk', async()=>{
    const p=await picker({catalog:[conf,{tag:'vienna',talks:[]}]});
    p.change('conference-choice','vienna');
    p.window.refreshBroadcastMatch();
    assert.equal(p.element('conference-choice').value,'vienna');
    assert.equal(p.element('broadcast-recording').value,'');
    assert.equal(p.element('broadcast-mode').value,'talk');
    p.change('broadcast-mode','conference');
    assert.equal(p.element('broadcast-conference').value,'vienna');
});
test('losing an automatic match clears visible selections as well as the submitted target', async()=>{
    const p=await picker();
    p.state.clips=['unmatched.mp4'];p.window.refreshBroadcastMatch();
    assert.equal(p.element('talk-choice').value,'');
    assert.equal(p.element('conference-choice').value,'');
    assert.equal(p.element('broadcast-recording').value,'');
    assert.equal(p.element('talk-choices').hidden,false);
});
