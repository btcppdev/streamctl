const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// Exercise the shared player, including its actual DOM event handlers. Media
// events are explicit here so slow requests and rounded clocks are deterministic.
function player() {
  const timers = new Map();
  let timerID = 0;
  class Element {
    constructor() {
      this.children = new Map();
      this.attributes = {};
      this.style = {setProperty() {}};
      this.classList = {add() {}, remove() {}};
      this.textContent = '';
      this.value = '';
    }
    querySelector(selector) {
      if (!this.children.has(selector)) this.children.set(selector, new Element());
      return this.children.get(selector);
    }
    setAttribute(key, value) { this.attributes[key] = value; }
    getAttribute(key) { return this.attributes[key] || null; }
    removeAttribute(key) { delete this.attributes[key]; }
    getBoundingClientRect() { return {width: 0, height: 62}; }
    focus() {}
    setPointerCapture() {}
    hasPointerCapture() { return false; }
    insertBefore() {}
    remove() {}
  }
  class Video extends Element {
    constructor() {
      super();
      this.readyState = 0;
      this.paused = true;
      this.seeking = false;
      this.clock = 0;
      this.seeks = [];
      this.loads = 0;
    }
    get currentTime() { return this.clock; }
    set currentTime(value) {
      this.seeks.push(value);
      this.clock = value;
      this.seeking = true;
    }
    get src() { return this.attributes.src; }
    set src(value) { this.attributes.src = value; }
    load() { this.loads++; this.readyState = 0; this.seeking = false; this.clock = 0; this.error = null; }
    pause() { this.paused = true; this.onpause?.(); }
    play() { this.paused = false; this.onplay?.(); return Promise.resolve(); }
    metadata() { this.readyState = 1; this.onloadedmetadata(); }
    canplay() { this.readyState = 2; this.oncanplay(); }
    settled(time = this.clock) {
      this.clock = time;
      this.seeking = false;
      this.readyState = 2;
      this.onseeked();
    }
  }
  const root = new Element(), video = new Video(), videos = [video];
  root.children.set('video', video);
  const context = {
    document: {documentElement: {}, activeElement: null, createElement() { const node = new Video(); videos.push(node); return node; }},
    window: {addEventListener() {}},
    getComputedStyle: () => ({getPropertyValue: () => ''}),
    requestAnimationFrame: () => 1, cancelAnimationFrame() {},
    setTimeout(fn) { timers.set(++timerID, fn); return timerID; },
    clearTimeout(id) { timers.delete(id); },
    URLSearchParams,
    fetch: async () => ({ok: true, json: async () => ({proxyPath: 'berlin25/recordings/workspace/day1.proxy.mp4', durationMs: 27_143_240})}),
  };
  vm.createContext(context);
  const html = fs.readFileSync(path.join(__dirname, '../templates/production_media_preview.html'), 'utf8');
  vm.runInContext(html.match(/<script>([\s\S]*?)<\/script>/)[1], context);
  const preview = context.createProductionMediaPreview(root);
  return {
    preview, video, videos, root,
    async load(at = 0) { await preview.loadVideo('berlin25/recordings/raw/mix/day1.mp4', 'video', at); },
    async ready() { await this.load(); video.metadata(); video.canplay(); },
    expireTimers() { const pending = [...timers.values()]; timers.clear(); pending.forEach(fn => fn()); },
  };
}

test('slow seeks retain the resource and destination instead of reloading', async () => {
  const p = player();
  await p.ready();
  p.preview.seek(26_400_000);
  p.expireTimers();
  assert.equal(p.video.loads, 1);
  assert.equal(p.video.seeks.at(-1), 26_400);
  assert.equal(p.preview.ready(), false);
  p.video.settled();
  assert.equal(p.preview.ready(), true);
});

test('seeked completes when the media reports an adjusted position', async () => {
  const p = player();
  await p.ready();
  p.preview.seek(12_345);
  p.video.settled(12.3);
  assert.deepEqual(p.video.seeks, [12.345]);
  assert.equal(p.preview.ready(), true);
  assert.equal(p.preview.currentTime(), 12_300);
});

test('source switching ignores stale events and reuses the cached media element', async () => {
  const p = player();
  await p.ready();
  p.preview.seek(1_000);
  await p.preview.loadVideo('berlin25/recordings/raw/mix/day2.mp4', 'video', 50_000);
  const secondVideo = p.videos[1];
  secondVideo.metadata();
  p.video.settled(); // Delayed event from the now-inactive first source.
  assert.equal(p.preview.ready(), false);
  assert.deepEqual(secondVideo.seeks, [50]);
  secondVideo.settled();
  assert.equal(p.preview.ready(), true);
  await p.load(2_000);
  assert.equal(p.video.loads, 1);
  assert.equal(secondVideo.loads, 1);
  assert.deepEqual(p.video.seeks, [1, 2]);
  p.video.settled();
  assert.equal(p.preview.ready(), true);
});

test('rapid requests coalesce to the latest destination', async () => {
  const p = player();
  await p.ready();
  p.preview.seek(1_000);
  p.preview.seek(15_000);
  p.preview.seek(26_400_000);
  assert.deepEqual(p.video.seeks, [1]);
  p.video.settled();
  assert.deepEqual(p.video.seeks, [1, 26_400]);
  p.video.settled();
  assert.equal(p.preview.ready(), true);
});

test('initial position is requested at metadata, without waiting for the first frame', async () => {
  const p = player();
  await p.load(26_400_000);
  p.video.metadata();
  assert.deepEqual(p.video.seeks, [26_400]);
  p.video.settled();
  assert.equal(p.preview.ready(), true);
});

test('requests received while metadata loads are retained', async () => {
  const p = player();
  await p.load(5_000);
  p.preview.seek(26_400_000);
  p.video.metadata();
  assert.deepEqual(p.video.seeks, [26_400]);
});

test('requests during initial loading at zero are retained too', async () => {
  const p = player();
  await p.load();
  p.preview.seek(1_234);
  p.video.metadata();
  assert.deepEqual(p.video.seeks, [1.234]);
});

test('a network error refreshes the URL once and retains the latest destination', async () => {
  const p = player();
  await p.ready();
  const firstURL = p.video.src;
  p.preview.seek(5_000);
  p.preview.seek(26_400_000);
  p.video.error = {code: 2};
  p.video.onerror();
  assert.equal(p.video.loads, 2);
  assert.notEqual(p.video.src, firstURL);
  p.video.metadata();
  assert.equal(p.video.seeks.at(-1), 26_400);
  p.video.settled();
  assert.equal(p.preview.ready(), true);
});

test('a repeated load failure stops retrying without resetting the requested position', async () => {
  const p = player();
  await p.load(26_400_000);
  for (let i = 0; i < 2; i++) {
    p.video.error = {code: 4};
    p.video.onerror();
  }
  assert.equal(p.video.loads, 2);
  assert.match(p.root.querySelector('.media-preview-status').textContent, /Press Play to retry/);
  assert.equal(p.preview.ready(), false);
  p.preview.togglePlay();
  assert.equal(p.video.loads, 3);
  p.video.metadata();
  assert.equal(p.video.seeks.at(-1), 26_400);
  p.video.settled();
  assert.equal(p.video.paused, false);
});

test('a decode error is surfaced without an automatic reload', async () => {
  const p = player();
  await p.ready();
  p.video.error = {code: 3};
  p.video.onerror();
  assert.equal(p.video.loads, 1);
  assert.equal(p.preview.ready(), false);
});

test('loading metadata without a frame does not reset the automatic retry limit', async () => {
  const p = player();
  await p.load();
  for (let i = 0; i < 2; i++) {
    p.video.metadata();
    p.video.error = {code: 2};
    p.video.onerror();
  }
  assert.equal(p.video.loads, 2);
});

test('a failed cached source can be selected again', async () => {
  const p = player();
  await p.ready();
  p.video.error = {code: 3};
  p.video.onerror();
  await p.load(12_000);
  assert.equal(p.video.loads, 2);
  p.video.metadata();
  assert.equal(p.video.seeks.at(-1), 12);
});

test('ribbon scrubbing stays paused while coarse scrubbing resumes playback', async () => {
  for (const ribbon of [true, false]) {
    const p = player();
    await p.ready();
    p.video.clock = 10;
    p.preview.togglePlay();
    const element = p.root.querySelector(ribbon ? '.media-preview-fine' : '.media-preview-seek');
    element.getBoundingClientRect = () => ({width: 500, height: 62});
    // Avoid drawing on the fake canvas while giving drag calculations a width.
    p.root.querySelector('.media-preview-fine').querySelector('canvas').getContext = () => new Proxy({}, {get: () => () => {}});
    const event = {button: 0, pointerId: 1, clientX: 100, preventDefault() {}};
    element.onpointerdown(event);
    if (ribbon) element.onpointermove({...event, clientX: 200});
    else { element.value = 9_000; element.oninput(); }
    element.onpointerup(event);
    p.video.settled();
    assert.equal(p.preview.ready(), true);
    assert.equal(p.video.paused, ribbon);
  }
});
