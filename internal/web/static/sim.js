// Simulator page only (templates/simulator.html). The server sends every
// step as HTML; this file only animates what it sends:
// the newest terminal block (data-type) is typed out line by line with
// typewriter-effect, and the terminal scrolls to follow it. Under
// prefers-reduced-motion the text appears at once.
//
// app.css hides a data-type block until it gets data-typed here, so its
// full text never flashes before typing starts (htmx:load comes after
// htmx's settle delay). sim-noscript.css shows it when JavaScript is off.
//
// Terminal text must never contain user input. typewriter-effect parses a
// string that looks like an HTML tag (/<[a-z][\s\S]*>/i) as HTML through
// innerHTML, and we hand it textContent, which the browser has already
// unescaped. Today every line is built in simulator.go from fixed names,
// descriptions, numbers and a checked currency code.
const reduce = matchMedia("(prefers-reduced-motion: reduce)");
const seen = new WeakSet();
let finishTyping = null; // shows the rest of the block being typed

// typewriter-effect types one chunk per animation frame (about 60 a
// second), so a long block is typed a few characters at a time to finish
// within the server's budget (data-type-ms on the terminal). Each line also
// costs about two frames: one to start and one to move to the next line.
function chunker(texts, budgetMs) {
  const chars = texts.reduce((n, t) => n + [...t].length, 0);
  const frames = Math.max(1, Math.floor(budgetMs / (1000 / 60)) - 2 * texts.length);
  const size = Math.max(1, Math.ceil(chars / frames));
  return (s) => {
    const cps = [...s];
    const out = [];
    for (let i = 0; i < cps.length; i += size) out.push(cps.slice(i, i + size).join(""));
    return out;
  };
}

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
    block.dataset.typed = "";
    finish();
    return;
  }
  finishTyping = finish;
  lines.forEach((l) => { l.textContent = ""; });
  block.dataset.typed = "";
  follow(term);
  const stringSplitter = chunker(texts, Number(term?.dataset.typeMs) || 1500);
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
      stringSplitter,
      onCreateTextNode: (_, node) => { follow(term); return node; },
    });
    tw.typeString(texts[i++]).callFunction(next).start();
  };
  next();
}

function wire(root) {
  for (const block of all(root, ".terminal-block[data-type]")) {
    if (!seen.has(block)) {
      seen.add(block);
      typeBlock(block);
    }
  }
}

wire(document);
document.addEventListener("htmx:load", (e) => wire(e.target));
