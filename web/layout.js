// Stage geometry: where the device box goes inside the stage, and where a
// piece of skin art goes once it does.
//
// This is the part of fitScreen() that has no DOM in it -- contain fit,
// corner reservation, the wide-device lift -- split out so it can be tested
// without a document (app.js still has no harness, trap 21) and so the same
// numbers are available to a future KMP client via testdata/client/layout.json.

function positive(n) {
  return typeof n === 'number' && isFinite(n) && n > 0;
}

// fitStage returns where the device box goes inside the stage, in CSS
// pixels, or null when the stage or the box has no size yet -- the same
// early return fitScreen made when canvas.width/height or the stage's own
// box were still zero.
//
//   stage    {width, height}         the stage's content box (padding already removed)
//   box      {width, height}         frame size if a frame is shown, else the stream size
//   corners  {width, height, gap}    a corner cluster's own footprint: `width`
//                                    is what it costs sitting beside the
//                                    device (a gutter, both bottom corners
//                                    assumed the same width), `height` is what
//                                    it costs sitting below the device (the
//                                    band), and `gap` is kept clear on top of
//                                    either. The two are independent because
//                                    the cluster itself is not square -- see
//                                    style.css's --corner-w/--corner-h -- and
//                                    conflating them (an earlier version used
//                                    one `size` for both) is what made the
//                                    lifted, banded fit win even when a
//                                    narrow gutter beside the device had
//                                    plenty of room: the reservation it was
//                                    tested against was the cluster's wider,
//                                    unstacked footprint rather than its
//                                    actual one.
//
// Contain fit on both axes (trap 18): the whole device is always reachable,
// which is the point of a fit that never crops. Centred horizontally,
// top-aligned to the stage's own top -- there is no vertical centring, so a
// device that ends up shorter than the stage sits at the top rather than
// floating in the middle.
//
// The corner clusters sit at the page's own bottom corners, in front of
// whatever the stage draws underneath them. Two fits are always candidates,
// and fitStage takes whichever is larger rather than assuming one:
//
//   scaleA  full stage height, device narrowed to fit the gap *between* the
//           two clusters (`clearWidth`, using `corners.width`) -- band: false
//   scaleB  full stage width, device shortened to sit *above* the clusters
//           (`availH`, using `corners.height`) -- band: true
//
// Picking the larger is what makes `band` mean "which computation won"
// rather than "did the device get smaller": a box whose full-height fit
// already clears the clusters makes scaleA the larger option on its own,
// with no separate check needed for whether it was ever in danger of the
// band. Earlier this only ever computed scaleB once the full-height fit
// collided with the clusters, which meant a box that was too wide to pass
// between them always lost the height it could have kept, even when sitting
// above the clusters would have scaled it down by more.
//
// Either candidate is excluded rather than computed when its own space is
// non-positive (clusters meeting or overlapping in the middle, or a stage
// shorter than the cluster's height plus its gap) -- a negative scale is not
// a smaller option, it is not an option. If both are excluded the clusters
// consume the whole stage and there is no reservation left to honour: fall
// back to a plain contain fit (band: false) rather than refusing to place
// the device.
export function fitStage(stage, box, corners) {
  if (!stage || !box || !positive(stage.width) || !positive(stage.height)) return null;
  if (!positive(box.width) || !positive(box.height)) return null;

  const c = corners || { width: 0, height: 0, gap: 0 };
  const cWidth = positive(c.width) ? c.width : 0;
  const cHeight = positive(c.height) ? c.height : 0;
  const gap = positive(c.gap) ? c.gap : 0;

  const clearWidth = stage.width - 2 * cWidth - 2 * gap;
  const availH = stage.height - cHeight - gap;

  const scaleA = clearWidth > 0
    ? Math.min(clearWidth / box.width, stage.height / box.height)
    : null;
  const scaleB = availH > 0
    ? Math.min(stage.width / box.width, availH / box.height)
    : null;

  let scale, band;
  if (scaleA === null && scaleB === null) {
    scale = Math.min(stage.width / box.width, stage.height / box.height);
    band = false;
  } else if (scaleB === null || (scaleA !== null && scaleA >= scaleB)) {
    scale = scaleA;
    band = false;
  } else {
    scale = scaleB;
    band = true;
  }

  // Floor, not round: a fractional stage (e.g. a 370.5px layout box) must
  // never let the device box round up past what the stage actually has.
  const width = Math.floor(box.width * scale);
  const height = Math.floor(box.height * scale);
  const x = Math.round((stage.width - width) / 2);
  const y = 0;

  return { scale, x, y, width, height, band };
}

// placeArt is place()'s arithmetic without the DOM write: where a piece of
// skin art goes once it is turned. x, y, w, h are the art's natural
// position and size on its own layout grid (trap 40 -- never stretched to a
// box); scale is fitStage's own scale. turnedArt is passed in rather than
// imported, because only app.js is allowed to import a sibling module (rule
// 1) and orientation.js already owns this arithmetic. gridW and gridH are the
// layout the art is drawn on, which the turn pivots on rather than the image
// (see turnedArt); they default to the art's own size, right for the mask.
export function placeArt(x, y, w, h, scale, turn, turnedArt, gridW = w, gridH = h) {
  const box = turnedArt(turn, Math.round(w * scale), Math.round(h * scale),
    Math.round(gridW * scale), Math.round(gridH * scale));
  return {
    left: Math.round(x * scale),
    top: Math.round(y * scale),
    width: box.width,
    height: box.height,
    transform: box.transform,
  };
}

// gridCells splits the stage's content box into one cell per pane when
// several are open side by side (Figma 59:178, 59:761, 59:1374): two or
// three panes share one row, four go two by two. Cells are equal and `gap`
// apart; each pane then contain-fits its device inside its own cell.
//
//   count  number of panes, 1 to 4
//   box    {x, y, width, height}   the stage's content box
//   gap    px between cells
export function gridCells(count, box, gap = 0) {
  if (!(count > 0) || !box || !positive(box.width) || !positive(box.height)) return [];
  if (count === 1) return [box];
  const cols = count === 4 ? 2 : count;
  const rows = Math.ceil(count / cols);
  const width = Math.floor((box.width - gap * (cols - 1)) / cols);
  const height = Math.floor((box.height - gap * (rows - 1)) / rows);
  const cells = [];
  for (let i = 0; i < count; i++) {
    const col = i % cols, row = Math.floor(i / cols);
    // The last column and row take the rounding, so the grid ends on the box.
    const x = col === cols - 1 ? box.x + box.width - width : box.x + col * (width + gap);
    const y = row === rows - 1 ? box.y + box.height - height : box.y + row * (height + gap);
    cells.push({ x, y, width, height });
  }
  return cells;
}
