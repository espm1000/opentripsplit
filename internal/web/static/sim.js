// Simulator page only (templates/simulator.html). The server sends every
// step as HTML; this file only animates what it sends:
// - the newest terminal block (data-type) is typed out line by line with
//   typewriter-effect, and the terminal scrolls to follow it
// - AutoAnimate slides new blocks into the terminal
// Under prefers-reduced-motion the text appears at once (AutoAnimate checks
// the setting itself).
import autoAnimate from "./auto-animate.min.js";

const reduce = matchMedia("(prefers-reduced-motion: reduce)");
const seen = new WeakSet();
let finishTyping = null; // shows the rest of the block being typed

function all(root, selector) {
  const found = [...root.querySelectorAll(selector)];
  return root.matches?.(selector) ? [root, ...found] : found;
}

function follow(term) {
  if (term) term.scrollTop = term.scrollHeight;
}

function typeBlock(block) {
  finishTyping?.();
  const term = block.closest(".terminal-lines");
  const lines = [...block.querySelectorAll(".terminal-line")];
  const texts = lines.map((l) => l.textContent);
  let tw = null;
  const finish = () => {
    tw?.stop();
    lines.forEach((l, i) => { l.textContent = texts[i]; });
    follow(term);
    finishTyping = null;
  };
  if (reduce.matches || !globalThis.Typewriter) {
    finish();
    return;
  }
  finishTyping = finish;
  lines.forEach((l) => { l.textContent = ""; });
  let i = 0;
  const next = () => {
    lines[i - 1]?.querySelector(".Typewriter__cursor")?.remove();
    if (i === lines.length) {
      finishTyping = null;
      return;
    }
    tw = new globalThis.Typewriter(lines[i], {
      delay: 6,
      cursor: "█",
      skipAddStyles: true, // its <style> tag would be blocked by the CSP
      onCreateTextNode: (_, node) => { follow(term); return node; },
    });
    tw.typeString(texts[i++]).callFunction(next).start();
  };
  next();
}

function wire(root) {
  for (const el of all(root, "[data-auto-animate]")) {
    if (!seen.has(el)) {
      seen.add(el);
      autoAnimate(el);
    }
  }
  for (const block of all(root, ".terminal-block[data-type]")) {
    if (!seen.has(block)) {
      seen.add(block);
      typeBlock(block);
    }
  }
}

wire(document);
document.addEventListener("htmx:load", (e) => wire(e.target));
