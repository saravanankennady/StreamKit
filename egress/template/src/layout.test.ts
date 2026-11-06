import assert from 'node:assert/strict';
import { it } from 'node:test';
import { fitSource, gridLayout, speakerLayout } from './layout.ts';

const within = (rects: { x: number; y: number; w: number; h: number }[], W: number, H: number) => {
  for (const r of rects) {
    assert.ok(r.x >= -0.01 && r.y >= -0.01 && r.x + r.w <= W + 0.01 && r.y + r.h <= H + 0.01, JSON.stringify(r));
  }
};
const overlap = (a: any, b: any) => a.x < b.x + b.w - 0.01 && b.x < a.x + a.w - 0.01 && a.y < b.y + b.h - 0.01 && b.y < a.y + a.h - 0.01;

it('grid tiles stay on the canvas and never overlap, for any count', () => {
  for (let n = 0; n <= 25; n++) {
    const rects = gridLayout(Array.from({ length: n }, (_, i) => i), 1280, 720);
    assert.equal(rects.length, n);
    within(rects, 1280, 720);
    for (let i = 0; i < n; i++) for (let j = i + 1; j < n; j++) assert.ok(!overlap(rects[i], rects[j]), `n=${n} ${i},${j}`);
  }
});

it('one tile fills the canvas, and two sit side by side', () => {
  const [one] = gridLayout(['a'], 1280, 720);
  assert.equal(Math.round(one.w), 1280);
  assert.equal(Math.round(one.h), 720);
  const two = gridLayout(['a', 'b'], 1280, 720);
  assert.equal(two[0].y, two[1].y);
  assert.ok(two[1].x > two[0].x);
});

it('speaker layout gives the main tile most of the canvas', () => {
  const rects = speakerLayout('main', ['a', 'b', 'c'], 1280, 720);
  assert.equal(rects[0].tile, 'main');
  assert.ok(rects[0].w * rects[0].h > 0.7 * 1280 * 720);
  within(rects, 1280, 720);
  for (let i = 1; i < rects.length; i++) assert.ok(!overlap(rects[0], rects[i]));
});

it('speaker layout with nobody else is the main tile alone, and with no speaker falls back to a grid', () => {
  assert.equal(speakerLayout('main', [], 1280, 720).length, 1);
  assert.equal(speakerLayout(undefined, ['a', 'b'], 1280, 720).length, 2);
  assert.equal(speakerLayout(undefined, [], 1280, 720).length, 0);
});

it('cover crops the source and letterbox keeps all of it', () => {
  const rect = { x: 0, y: 0, w: 640, h: 360 };
  const cropped = fitSource(640, 480, rect, true);
  assert.equal(cropped.dw, 640);
  assert.equal(cropped.dh, 360);
  assert.ok(cropped.sh < 480);
  const boxed = fitSource(640, 480, rect, false);
  assert.equal(boxed.sh, 480);
  assert.equal(boxed.dh, 360);
  assert.ok(boxed.dw < 640);
});
