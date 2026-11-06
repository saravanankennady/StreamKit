// Where each video goes on the canvas. Pure, so it can be tested without a browser.

export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface Tile {
  key: string;
  isScreen: boolean;
}

export interface Placed<T> extends Rect {
  tile: T;
}

const GAP = 4;
const ASPECT = 16 / 9;

/** Rows of equal 16:9 tiles, centred. Three tiles in a two by two grid leave the last row centred too. */
export function gridLayout<T>(tiles: T[], width: number, height: number): Placed<T>[] {
  const n = tiles.length;
  if (n === 0) return [];
  const area = (cols: number) => {
    const rows = Math.ceil(n / cols);
    const cellW = (width - GAP * (cols - 1)) / cols;
    const cellH = (height - GAP * (rows - 1)) / rows;
    return Math.min(cellW, cellH * ASPECT);
  };
  // Pick the column count that gives the largest tiles.
  let cols = 1;
  for (let c = 2; c <= n; c++) if (area(c) > area(cols)) cols = c;
  const rows = Math.ceil(n / cols);
  const tileW = area(cols);
  const tileH = tileW / ASPECT;
  const totalH = rows * tileH + (rows - 1) * GAP;
  const out: Placed<T>[] = [];
  for (let i = 0; i < n; i++) {
    const row = Math.floor(i / cols);
    const inRow = row === rows - 1 ? n - cols * (rows - 1) : cols;
    const rowW = inRow * tileW + (inRow - 1) * GAP;
    const col = i - row * cols;
    out.push({
      tile: tiles[i],
      x: (width - rowW) / 2 + col * (tileW + GAP),
      y: (height - totalH) / 2 + row * (tileH + GAP),
      w: tileW,
      h: tileH,
    });
  }
  return out;
}

/** One large tile, with the others in a strip along the bottom. */
export function speakerLayout<T>(main: T | undefined, others: T[], width: number, height: number): Placed<T>[] {
  if (!main) return gridLayout(others, width, height);
  if (others.length === 0) return [{ tile: main, x: 0, y: 0, w: width, h: height }];
  const shown = others.slice(0, 5);
  const stripH = Math.round(height / 5);
  const out: Placed<T>[] = [{ tile: main, x: 0, y: 0, w: width, h: height - stripH - GAP }];
  const tileW = stripH * ASPECT;
  const rowW = shown.length * tileW + (shown.length - 1) * GAP;
  shown.forEach((tile, i) => {
    out.push({ tile, x: (width - rowW) / 2 + i * (tileW + GAP), y: height - stripH, w: tileW, h: stripH });
  });
  return out;
}

/** Crops a source to fill a rectangle, as CSS object-fit: cover does. Screens letterbox instead: text must not be cut. */
export function fitSource(srcW: number, srcH: number, r: Rect, cover: boolean) {
  const src = srcW / srcH;
  const dst = r.w / r.h;
  if (cover) {
    if (src > dst) {
      const w = srcH * dst;
      return { sx: (srcW - w) / 2, sy: 0, sw: w, sh: srcH, dx: r.x, dy: r.y, dw: r.w, dh: r.h };
    }
    const h = srcW / dst;
    return { sx: 0, sy: (srcH - h) / 2, sw: srcW, sh: h, dx: r.x, dy: r.y, dw: r.w, dh: r.h };
  }
  if (src > dst) {
    const h = r.w / src;
    return { sx: 0, sy: 0, sw: srcW, sh: srcH, dx: r.x, dy: r.y + (r.h - h) / 2, dw: r.w, dh: h };
  }
  const w = r.h * src;
  return { sx: 0, sy: 0, sw: srcW, sh: srcH, dx: r.x + (r.w - w) / 2, dy: r.y, dw: w, dh: r.h };
}
