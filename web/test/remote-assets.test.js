import test from 'node:test';
import assert from 'node:assert/strict';
import { createRemoteAssetTransport } from '../js/bootstrap.js';

test('remote chat images arrive over the channel before the chat renders', async () => {
  const path = '/api/nodes/drive/assets/a_plot';
  const calls = [];
  const blobs = [];
  const transport = createRemoteAssetTransport({
    channel: async url => {
      calls.push(url);
      if (url === path) return new Response(new Uint8Array([137,80,78,71]), {headers: {'Content-Type':'image/png'}});
      return new Response(JSON.stringify({assets: {a_plot: {url: path}}, turns: []}));
    },
    assets: new Map([['/icon.svg', 'blob:icon']]),
    createObjectURL: blob => { blobs.push(blob); return 'blob:plot'; },
  });
  await (await transport.fetchImpl('/api/nodes/drive/chat')).json();
  assert.equal(transport.assetURL(path), 'blob:plot');
  assert.equal(blobs[0].type, 'image/png');
  assert.deepEqual([...new Uint8Array(await blobs[0].arrayBuffer())], [137,80,78,71]);
  await (await transport.fetchImpl('/api/nodes/drive/chat')).json();
  assert.equal(calls.filter(url => url === path).length, 1);
  assert.equal(transport.assetURL('/icon.svg'), 'blob:icon');
  assert.equal(transport.assetURL('/missing'), '');
});

test('missing assets can retry and external asset URLs never leave the channel', async () => {
  let attempts = 0;
  const path = '/api/nodes/drive/assets/a_plot';
  const transport = createRemoteAssetTransport({
    channel: async url => {
      if (url === path) return ++attempts === 1 ? new Response('', {status:404}) : new Response('png');
      assert.equal(url, '/api/nodes/drive/chat');
      return new Response(JSON.stringify({assets: {a: {url:path}, b: {url:'https://elsewhere/image.png'}}}));
    }, assets: new Map(), createObjectURL: () => 'blob:retry',
  });
  await (await transport.fetchImpl('/api/nodes/drive/chat')).json();
  assert.equal(transport.assetURL(path), '');
  await (await transport.fetchImpl('/api/nodes/drive/chat')).json();
  assert.equal(transport.assetURL(path), 'blob:retry');
});
