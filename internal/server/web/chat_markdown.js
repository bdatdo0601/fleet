import { el } from "./lib.js";

/**
 * Sanitize a markdown link href, permitting only http(s) URLs and in-page # anchors.
 */
export function sanitizeLinkHref(href) {
  const trimmed = String(href || "").trim();
  if (!trimmed) {
    return "";
  }
  if (trimmed.startsWith("#")) {
    return /^#[^\s]*$/.test(trimmed) ? trimmed : "";
  }
  try {
    const parsed = new URL(trimmed);
    if (parsed.protocol === "http:" || parsed.protocol === "https:") {
      return parsed.href;
    }
  } catch {
    return "";
  }
  return "";
}

/**
 * Tokenize inline markdown (code spans, bold, and sanitized links) into plain objects.
 */
export function tokenizeInlineMarkdown(text) {
  const input = String(text || "");
  const tokens = [];
  const re = /(`[^`]+`|\*\*[^*]+\*\*|\[([^\]]+)\]\(([^)\s]+)\))/g;
  let last = 0;
  let m;
  while ((m = re.exec(input)) !== null) {
    if (m.index > last) {
      tokens.push({ type: "text", text: input.slice(last, m.index) });
    }
    const tok = m[0];
    if (tok.startsWith("`") && tok.endsWith("`")) {
      tokens.push({ type: "code", text: tok.slice(1, -1) });
    } else if (tok.startsWith("**") && tok.endsWith("**")) {
      tokens.push({ type: "strong", text: tok.slice(2, -2) });
    } else {
      const safeHref = sanitizeLinkHref(m[3]);
      if (safeHref) {
        tokens.push({
          type: "link",
          text: m[2],
          href: safeHref,
          external: !safeHref.startsWith("#"),
        });
      } else {
        tokens.push({ type: "text", text: tok });
      }
    }
    last = re.lastIndex;
  }
  if (last < input.length) {
    tokens.push({ type: "text", text: input.slice(last) });
  }
  return tokens;
}

/**
 * Parse markdown text into block descriptors (code fences, headings, lists, paragraphs).
 */
export function parseMarkdownBlocks(raw) {
  const text = String(raw || "");
  if (!text) {
    return [];
  }
  const out = [];
  const parts = text.split(/```/);
  for (let i = 0; i < parts.length; i++) {
    if (i % 2 === 1) {
      const lines = parts[i].split("\n");
      const codeBody = lines.length > 1 ? lines.slice(1).join("\n") : parts[i];
      out.push({ type: "code", text: codeBody.replace(/\n$/, "") });
      continue;
    }
    const blocks = parts[i].split(/\n{2,}/);
    for (const blk of blocks) {
      const trimmed = blk.trim();
      if (!trimmed) {
        continue;
      }
      const lines = trimmed.split("\n");
      if (lines.length === 1 && /^#{1,3}\s+/.test(lines[0])) {
        out.push({ type: "heading", text: lines[0].replace(/^#{1,3}\s+/, "") });
        continue;
      }
      if (lines.every((l) => /^\s*[-*]\s+/.test(l))) {
        out.push({ type: "list", items: lines.map((l) => l.replace(/^\s*[-*]\s+/, "")) });
      } else {
        out.push({ type: "paragraph", lines });
      }
    }
  }
  return out;
}

export function renderInlineDOM(text) {
  const frag = document.createDocumentFragment();
  for (const tok of tokenizeInlineMarkdown(text)) {
    if (tok.type === "code") {
      frag.appendChild(el("code", { className: "inline-code", text: tok.text }));
    } else if (tok.type === "strong") {
      frag.appendChild(el("strong", { text: tok.text }));
    } else if (tok.type === "link") {
      const props = tok.external
        ? { href: tok.href, target: "_blank", rel: "noopener noreferrer", text: tok.text }
        : { href: tok.href, text: tok.text };
      frag.appendChild(el("a", props));
    } else {
      frag.appendChild(document.createTextNode(tok.text));
    }
  }
  return frag;
}

export function renderMarkdownDOM(raw) {
  const wrap = el("div", { className: "chat-md" });
  for (const blk of parseMarkdownBlocks(raw)) {
    if (blk.type === "code") {
      wrap.appendChild(el("pre", { className: "code-block" }, el("code", { text: blk.text })));
    } else if (blk.type === "heading") {
      wrap.appendChild(el("h4", { className: "chat-h" }, renderInlineDOM(blk.text)));
    } else if (blk.type === "list") {
      const ul = el("ul", { className: "chat-list" });
      for (const item of blk.items) {
        ul.appendChild(el("li", null, renderInlineDOM(item)));
      }
      wrap.appendChild(ul);
    } else if (blk.type === "paragraph") {
      const p = el("p", { className: "chat-p" });
      blk.lines.forEach((l, idx) => {
        if (idx > 0) {
          p.appendChild(el("br"));
        }
        p.appendChild(renderInlineDOM(l));
      });
      wrap.appendChild(p);
    }
  }
  return wrap;
}
