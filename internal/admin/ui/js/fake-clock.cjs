"use strict";
// Deterministic timer queue shared by the sandbox; advance() fires due timers
// in order and drains promise jobs between them.
function fakeClock() {
  let now = 0;
  let seq = 0;
  const timers = new Map();
  async function flush() {
    for (let i = 0; i < 20; i++) await new Promise((resolve) => setImmediate(resolve));
  }
  return {
    setTimeout(fn, ms) {
      const id = ++seq;
      timers.set(id, {fn, at: now + Math.max(0, Number(ms) || 0)});
      return id;
    },
    clearTimeout(id) {
      timers.delete(id);
    },
    pending: () => timers.size,
    flush,
    async advance(ms) {
      const end = now + ms;
      for (;;) {
        await flush();
        let next = null;
        for (const [id, timer] of timers)
          if (timer.at <= end && (!next || timer.at < next[1].at)) next = [id, timer];
        if (!next) break;
        timers.delete(next[0]);
        now = next[1].at;
        next[1].fn();
      }
      now = end;
      await flush();
    },
  };
}

module.exports = {fakeClock};
