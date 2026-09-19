/**
 * Touch scrolling for the agent terminal (design §16).
 *
 * xterm 6 no longer has a natively scrollable viewport. The old
 * `.xterm-viewport` node is still rendered but is now an EMPTY box, and the real
 * scroll container is the JavaScript-driven `SmoothScrollableElement` whose own
 * `overflow` is `visible`; it reacts to wheel and to mouse drags on its scrollbar
 * only. On a touch device that leaves the terminal with nothing the browser can
 * pan, so a finger drag over the screen falls through to the nearest scrollable
 * ancestor — the page — and the scrollback can never be reached (the phone
 * report: "拖动的是整个页面，看不到之前的打印").
 *
 * So the terminal has to translate the gesture itself:
 *   • a single-finger drag is turned into `scrollLines()` calls, scaled by the
 *     measured row height so the text tracks the finger;
 *   • a release with speed becomes a decaying fling (a 10k-line scrollback is
 *     unusable without one);
 *   • the gesture is COOPERATIVE: it is only claimed while the scrollback can
 *     actually move the way the finger pulls. At either end of the buffer the
 *     drag is left to the page, which is what keeps the rest of the terminal
 *     page reachable on a phone. Never preventDefault an unclaimed gesture, or
 *     the page becomes impossible to scroll from the terminal area.
 *
 * Touch events (not pointer events) are used on purpose: `touch-action` is the
 * only other way to stop the browser panning, and it cannot be applied
 * conditionally per gesture direction.
 */
import type { Terminal } from '@xterm/xterm';

/** Movement (px) before the gesture is assigned to the terminal or the page. */
const DECIDE_PX = 4;
/** Vertical must beat horizontal by this factor to count as a scroll. */
const VERTICAL_BIAS = 1.2;
/** Fling shorter than this (px) is treated as a deliberate stop, not a flick. */
const FLING_MIN_TRAVEL_PX = 20;
/** Release speed below this (px/ms) does not start a fling. */
const FLING_MIN_SPEED = 0.25;
/** A wild flick is capped so it cannot throw the operator out of the buffer. */
const FLING_MAX_SPEED = 4;
/** Speed kept per 16ms frame once the finger is gone. */
const FLING_DECAY_PER_FRAME = 0.94;

type GestureOwner = 'idle' | 'undecided' | 'terminal' | 'page';

/**
 * Route vertical single-finger drags over `element` into `term`'s scrollback.
 * Returns a disposer; call it before the terminal itself is disposed.
 */
export function attachTouchScroll(term: Terminal, element: HTMLElement): () => void {
  let owner: GestureOwner = 'idle';
  let startX = 0;
  let startY = 0;
  let lastY = 0;
  let lastTime = 0;
  let speed = 0;
  // Fractional lines carried between events: a finger moves in pixels, the
  // buffer only moves in whole lines, and dropping the remainder on every event
  // makes a slow drag move nothing at all.
  let pendingLines = 0;
  let travel = 0;
  let flingFrame = 0;

  const stopFling = () => {
    if (flingFrame) {
      window.cancelAnimationFrame(flingFrame);
      flingFrame = 0;
    }
  };

  // Measured per gesture: a font or layout change while the operator stands
  // there would otherwise leave a stale row height that makes the drag drift.
  const rowHeight = () => {
    const rows = term.rows;
    const screen = element.querySelector<HTMLElement>('.xterm-screen');
    const height = screen?.clientHeight || element.clientHeight;
    if (rows < 1 || height <= 0) return 16;
    return height / rows;
  };

  /** dy > 0 means the finger moved down, i.e. the operator wants older lines. */
  const canScroll = (dy: number) => {
    const buffer = term.buffer.active;
    return dy > 0 ? buffer.viewportY > 0 : buffer.viewportY < buffer.baseY;
  };

  const scrollByPixels = (dy: number) => {
    pendingLines += dy / rowHeight();
    const whole = Math.trunc(pendingLines);
    if (whole === 0) return;
    pendingLines -= whole;
    // scrollLines() is positive = down = newer lines, the opposite of the finger.
    term.scrollLines(-whole);
  };

  const startFling = () => {
    let speedNow = Math.max(-FLING_MAX_SPEED, Math.min(FLING_MAX_SPEED, speed));
    if (Math.abs(speedNow) < FLING_MIN_SPEED) {
      owner = 'idle';
      return;
    }
    let previous = performance.now();
    const step = (now: number) => {
      // A backgrounded tab can hand back a huge delta; clamp it so the buffer
      // does not jump by thousands of lines on the way back.
      const delta = Math.min(64, now - previous);
      previous = now;
      speedNow *= Math.pow(FLING_DECAY_PER_FRAME, delta / 16);
      const dy = speedNow * delta;
      if (Math.abs(speedNow) < FLING_MIN_SPEED || !canScroll(dy)) {
        flingFrame = 0;
        owner = 'idle';
        return;
      }
      scrollByPixels(dy);
      flingFrame = window.requestAnimationFrame(step);
    };
    flingFrame = window.requestAnimationFrame(step);
  };

  const onTouchStart = (event: TouchEvent) => {
    stopFling();
    // Two fingers is a pinch or a zoom: never ours, and never prevented.
    if (event.touches.length !== 1) {
      owner = 'page';
      return;
    }
    const touch = event.touches[0];
    startX = touch.clientX;
    startY = touch.clientY;
    lastY = startY;
    lastTime = event.timeStamp;
    speed = 0;
    travel = 0;
    pendingLines = 0;
    owner = 'undecided';
  };

  const onTouchMove = (event: TouchEvent) => {
    if (owner === 'idle' || owner === 'page') return;
    if (event.touches.length !== 1) {
      owner = 'page';
      return;
    }
    const touch = event.touches[0];
    const y = touch.clientY;
    if (owner === 'undecided') {
      const dx = Math.abs(touch.clientX - startX);
      const dy = Math.abs(y - startY);
      if (dy < DECIDE_PX && dx < DECIDE_PX) return;
      // A mostly-horizontal drag has nothing to pan here — leave it to the page.
      if (dy < dx * VERTICAL_BIAS) {
        owner = 'page';
        return;
      }
      // lastY is still startY, so the first claimed step carries the whole
      // movement so far and the text does not lag behind by the slop.
      owner = canScroll(y - startY) ? 'terminal' : 'page';
      if (owner !== 'terminal') return;
    }
    // Claimed: this is the only place the browser is told to keep its hands off.
    event.preventDefault();
    const delta = Math.max(1, event.timeStamp - lastTime);
    const dy = y - lastY;
    speed = speed * 0.6 + (dy / delta) * 0.4;
    lastY = y;
    lastTime = event.timeStamp;
    travel += Math.abs(dy);
    scrollByPixels(dy);
  };

  const onTouchEnd = () => {
    if (owner !== 'terminal') {
      owner = 'idle';
      return;
    }
    // A flick has to travel a little first: a slow, deliberate drag that ends
    // with one fast event must not keep coasting on its own.
    if (travel >= FLING_MIN_TRAVEL_PX) startFling();
    else owner = 'idle';
  };

  const onTouchCancel = () => {
    stopFling();
    owner = 'idle';
  };

  // Capture phase: the handlers must see the gesture even if a child of the
  // terminal stops propagation, and touchmove has to be non-passive for
  // preventDefault to be allowed.
  element.addEventListener('touchstart', onTouchStart, { capture: true, passive: true });
  element.addEventListener('touchmove', onTouchMove, { capture: true, passive: false });
  element.addEventListener('touchend', onTouchEnd, { capture: true, passive: true });
  element.addEventListener('touchcancel', onTouchCancel, { capture: true, passive: true });

  return () => {
    stopFling();
    element.removeEventListener('touchstart', onTouchStart, { capture: true });
    element.removeEventListener('touchmove', onTouchMove, { capture: true });
    element.removeEventListener('touchend', onTouchEnd, { capture: true });
    element.removeEventListener('touchcancel', onTouchCancel, { capture: true });
  };
}
