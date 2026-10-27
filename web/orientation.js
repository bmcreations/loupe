// Whether the device bezel still describes the picture being streamed.
//
// A rotation changes the stream's shape without ending the session, so the
// frame fetched from /api/frame outlives the orientation it was fetched for.
// The server answers /api/frame for the orientation the device is in, so the
// frame that arrives after a rotation does suit the new shape. This covers the
// window before it arrives: the fetch is a round trip and the canvas has
// already changed. The caller drops the bezel on a no, which is what it did
// for the whole of the other orientation before the frame turned at all. It is
// decoration -- nothing about input or decode reads any of it -- and no bezel
// is the state the client starts in anyway.
//
// It is still the only guard on a derived frame, which does not turn. Its
// cutout is an untransformed path anchored along one edge by a dx offset, and
// that anchor means nothing once the axes swap.

// orientationOf names a rectangle's shape. Square is its own case because it
// agrees with both, and calling it one or the other would hide a frame that
// fits perfectly well.
export function orientationOf(w, h) {
  if (w > h) return 'landscape';
  if (w < h) return 'portrait';
  return 'square';
}

function positive(n) {
  return typeof n === 'number' && isFinite(n) && n > 0;
}

// frameSuitsStream answers whether frame's display rect can hold a streamW by
// streamH picture the right way round.
//
// It reads the display rect rather than the outer bezel because the display
// rect is where the canvas is placed; a bezel is free to be squarer than the
// screen inside it.
export function frameSuitsStream(frame, streamW, streamH) {
  if (!frame || !frame.display) return false;
  const d = frame.display;
  if (!positive(d.width) || !positive(d.height)) return false;
  if (!positive(streamW) || !positive(streamH)) return false;

  const want = orientationOf(streamW, streamH);
  const have = orientationOf(d.width, d.height);
  return want === have || want === 'square' || have === 'square';
}

// frameNeedsRecheck answers whether a frame on screen could be describing the
// wrong one of the two landscapes.
//
// A flip from 90 to 270 degrees leaves the stream exactly the shape it was, so
// nothing the video path carries says the bezel has to turn. Only a turned
// skin frame can be wrong this way: a portrait stream has one upright, and a
// derived frame does not turn at all and is hidden in landscape by
// frameSuitsStream. The caller asks the server again while this is true.
export function frameNeedsRecheck(frame, streamW, streamH) {
  if (!frame || frame.source !== 'skin') return false;
  if (!positive(streamW) || !positive(streamH)) return false;
  return orientationOf(streamW, streamH) === 'landscape' && frameSuitsStream(frame, streamW, streamH);
}

// turnedArt describes how to draw a piece of skin art turned a quarter turn
// clockwise, quarters times, keeping the top-left corner of its grid where it
// was.
//
// The element's own box stays the art's natural size, scaled. Skin art is
// authored on the layout's pixel grid (trap 40) and stretching it onto another
// box is exactly what put a punch hole on the outer edge of the bezel, so the
// turn is a transform and never a resize. boxW and boxH are that natural size
// already scaled to the screen, because a CSS translate is in pixels.
//
// Rotating about the top-left corner sweeps the image out of view -- by its
// grid's height on one turn and its grid's width on three -- so each turn
// carries the translate that puts it back. The two terms are different
// numbers, which is the part that looks like a typo and is not.
//
// gridW and gridH are the layout the art is drawn on, and they are what the
// turn pivots on rather than the image. The server turns the display rect
// about the layout's size, and back.webp is larger than its layout -- 1205x2535
// against 1198x2531 on pixel_10, the extra on the right and bottom. Pivoting on
// the image moved its hole 7 art px off the canvas at one turn and 4 at three,
// measured in a browser against emulator-5554. On the grid, the overhang turns
// out past the top or left edge instead, where the wrap clips it the way it
// already clips it on the right and bottom. They default to the box, which is
// right for the mask: it is exactly the display and has no overhang.
export function turnedArt(quarters, boxW, boxH, gridW = boxW, gridH = boxH) {
  const ok = positive(boxW) && positive(boxH) && positive(gridW) && positive(gridH);
  const q = ok ? (((Math.round(quarters) % 4) + 4) % 4) : 0;
  const shift = {
    1: `translate(${gridH}px, 0px)`,
    2: `translate(${gridW}px, ${gridH}px)`,
    3: `translate(0px, ${gridW}px)`,
  }[q];
  return {
    width: boxW,
    height: boxH,
    transform: shift ? `${shift} rotate(${q * 90}deg)` : '',
  };
}

// frameWantedFrom reads the saved `loupe.frame` value: unset means on (the Mac
// app shows the bezel by default), a stored '0' or '1' wins.
export function frameWantedFrom(stored) {
  if (stored === null || stored === undefined) return true;
  return stored === '1';
}
