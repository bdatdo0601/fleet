import { copyText, el } from "./lib.js";

const ESCAPABLE_CHARS = new Set(Array.from("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"));

/**
 * Sanitize a markdown link href, permitting http(s), mailto, same-origin relative paths, and #fragments.
 */
export function sanitizeLinkHref(rawUrl) {
  let s = String(rawUrl || "").trim();
  if (!s) {
    return "";
  }
  if (s.startsWith("<") && s.endsWith(">")) {
    s = s.slice(1, -1).trim();
  }
  if (!s || /[\x00-\x1f\x7f\s]/.test(s)) {
    return "";
  }
  if (s.startsWith("#")) {
    return s;
  }
  if (s.startsWith("/")) {
    if (s.startsWith("//") || s.startsWith("/\\")) {
      return "";
    }
    return s;
  }
  const colonIdx = s.indexOf(":");
  if (colonIdx > 0) {
    const scheme = s.slice(0, colonIdx).toLowerCase();
    if (scheme === "http" || scheme === "https") {
      const rest = s.slice(colonIdx + 1);
      if (!rest.startsWith("//") || rest.length <= 2) {
        return "";
      }
      return s;
    }
    if (scheme === "mailto") {
      return s.length > "mailto:".length ? s : "";
    }
    return "";
  }
  return "";
}

function isWordChar(ch) {
  return Boolean(ch && /[a-zA-Z0-9_]/.test(ch));
}

function isWhitespace(ch) {
  return !ch || /\s/.test(ch);
}

function expandTabs(str) {
  return str.replace(/\t/g, "  ");
}

function leadingIndent(line) {
  let indent = 0;
  for (let i = 0; i < line.length; i++) {
    if (line[i] === " ") {
      indent += 1;
    } else if (line[i] === "\t") {
      indent += 2;
    } else {
      break;
    }
  }
  return indent;
}

function stripIndent(line, cols) {
  let removed = 0;
  let idx = 0;
  while (idx < line.length && removed < cols) {
    if (line[idx] === " ") {
      removed += 1;
      idx += 1;
    } else if (line[idx] === "\t") {
      removed += 2;
      idx += 1;
    } else {
      break;
    }
  }
  return line.slice(idx);
}

function trimTrailingURLPunctuation(raw) {
  let url = raw;
  while (url.length > 0) {
    const last = url[url.length - 1];
    if (".,;:!?'\"*_~".includes(last)) {
      url = url.slice(0, -1);
      continue;
    }
    if (last === ")" || last === "]" || last === "}") {
      const openChar = last === ")" ? "(" : last === "]" ? "[" : "{";
      let openCount = 0;
      let closeCount = 0;
      for (const ch of url) {
        if (ch === openChar) {
          openCount += 1;
        } else if (ch === last) {
          closeCount += 1;
        }
      }
      if (closeCount > openCount) {
        url = url.slice(0, -1);
        continue;
      }
    }
    break;
  }
  return url;
}

function findExactBackticks(text, start, count) {
  let pos = start;
  while (pos < text.length) {
    const idx = text.indexOf("`", pos);
    if (idx === -1) {
      return -1;
    }
    let end = idx;
    while (end < text.length && text[end] === "`") {
      end += 1;
    }
    if (end - idx === count) {
      return idx;
    }
    pos = end;
  }
  return -1;
}

function findClosingBracket(text, openIdx) {
  let depth = 1;
  let pos = openIdx + 1;
  while (pos < text.length) {
    const ch = text[pos];
    if (ch === "\\" && pos + 1 < text.length) {
      pos += 2;
      continue;
    }
    if (ch === "`") {
      let run = 1;
      while (pos + run < text.length && text[pos + run] === "`") {
        run += 1;
      }
      const closeTicks = findExactBackticks(text, pos + run, run);
      if (closeTicks !== -1) {
        pos = closeTicks + run;
        continue;
      }
      pos += run;
      continue;
    }
    if (ch === "[") {
      depth += 1;
    } else if (ch === "]") {
      depth -= 1;
      if (depth === 0) {
        return pos;
      }
    }
    pos += 1;
  }
  return -1;
}

function parseLinkDestAndTitle(text, openParenIdx) {
  let pos = openParenIdx + 1;
  while (pos < text.length && (text[pos] === " " || text[pos] === "\t" || text[pos] === "\n")) {
    pos += 1;
  }
  if (pos >= text.length) {
    return null;
  }
  let dest = "";
  if (text[pos] === "<") {
    pos += 1;
    const start = pos;
    while (pos < text.length && text[pos] !== ">" && text[pos] !== "\n") {
      if (text[pos] === "\\" && pos + 1 < text.length) {
        pos += 2;
      } else {
        pos += 1;
      }
    }
    if (pos >= text.length || text[pos] !== ">") {
      return null;
    }
    dest = text.slice(start, pos);
    pos += 1;
  } else {
    let parenDepth = 0;
    const start = pos;
    while (pos < text.length) {
      const ch = text[pos];
      if (ch === "\\" && pos + 1 < text.length) {
        pos += 2;
        continue;
      }
      if (ch === "(") {
        parenDepth += 1;
        pos += 1;
        continue;
      }
      if (ch === ")") {
        if (parenDepth === 0) {
          break;
        }
        parenDepth -= 1;
        pos += 1;
        continue;
      }
      if (/\s/.test(ch)) {
        break;
      }
      pos += 1;
    }
    dest = text.slice(start, pos).replace(/\\([!"#$%&'()*+,-./:;<=>?@[\\\]^_`{|}~])/g, "$1");
  }

  while (pos < text.length && (text[pos] === " " || text[pos] === "\t" || text[pos] === "\n")) {
    pos += 1;
  }
  let title = "";
  if (pos < text.length && (text[pos] === "\"" || text[pos] === "'" || text[pos] === "(")) {
    const closeChar = text[pos] === "(" ? ")" : text[pos];
    pos += 1;
    const tStart = pos;
    while (pos < text.length && text[pos] !== closeChar) {
      if (text[pos] === "\\" && pos + 1 < text.length) {
        pos += 2;
      } else {
        pos += 1;
      }
    }
    if (pos >= text.length || text[pos] !== closeChar) {
      return null;
    }
    title = text.slice(tStart, pos).replace(/\\([!"#$%&'()*+,-./:;<=>?@[\\\]^_`{|}~])/g, "$1");
    pos += 1;
    while (pos < text.length && (text[pos] === " " || text[pos] === "\t" || text[pos] === "\n")) {
      pos += 1;
    }
  }
  if (pos >= text.length || text[pos] !== ")") {
    return null;
  }
  return { dest, title, endParenIdx: pos };
}

function findClosingDelimiter(text, start, delim) {
  const underscoreStyle = delim[0] === "_";
  let pos = start;
  while (pos < text.length) {
    const ch = text[pos];
    if (ch === "\\" && pos + 1 < text.length) {
      pos += 2;
      continue;
    }
    if (ch === "`") {
      let run = 1;
      while (pos + run < text.length && text[pos + run] === "`") {
        run += 1;
      }
      const closeTicks = findExactBackticks(text, pos + run, run);
      if (closeTicks !== -1) {
        pos = closeTicks + run;
        continue;
      }
      pos += run;
      continue;
    }
    if (text.startsWith(delim, pos)) {
      const prev = pos > 0 ? text[pos - 1] : "";
      const next = pos + delim.length < text.length ? text[pos + delim.length] : "";
      const validPrev = !isWhitespace(prev);
      const validNext = !underscoreStyle || !isWordChar(next);
      if (validPrev && validNext && pos > start) {
        return pos;
      }
    }
    pos += 1;
  }
  return -1;
}

const DELIMITERS = ["***", "___", "**", "__", "~~", "*", "_"];

function pushText(tokens, str) {
  if (!str) {
    return;
  }
  const prev = tokens[tokens.length - 1];
  if (prev && prev.type === "text") {
    prev.text += str;
  } else {
    tokens.push({ type: "text", text: str });
  }
}

/**
 * Tokenize inline markdown (code spans, emphasis, strikethrough, escapes, links, images, and autolinks).
 */
export function tokenizeInlineMarkdown(text, opts = {}) {
  const src = String(text || "");
  if (!src) {
    return [];
  }
  const depth = opts.depth || 0;
  const insideLink = Boolean(opts.insideLink);
  if (depth > 8) {
    return [{ type: "text", text: src }];
  }

  const tokens = [];
  let pos = 0;

  while (pos < src.length) {
    const ch = src[pos];

    if (ch === "\n") {
      tokens.push({ type: "br" });
      pos += 1;
      continue;
    }

    if (ch === "\\" && pos + 1 < src.length && ESCAPABLE_CHARS.has(src[pos + 1])) {
      pushText(tokens, src[pos + 1]);
      pos += 2;
      continue;
    }

    if (ch === "`") {
      let run = 1;
      while (pos + run < src.length && src[pos + run] === "`") {
        run += 1;
      }
      const closeIdx = findExactBackticks(src, pos + run, run);
      if (closeIdx !== -1) {
        let code = src.slice(pos + run, closeIdx).replace(/\n/g, " ");
        if (code.length >= 2 && code.startsWith(" ") && code.endsWith(" ") && /[^ ]/.test(code)) {
          code = code.slice(1, -1);
        }
        tokens.push({ type: "code", text: code });
        pos = closeIdx + run;
        continue;
      }
      pushText(tokens, src.slice(pos, pos + run));
      pos += run;
      continue;
    }

    if (ch === "!" && pos + 1 < src.length && src[pos + 1] === "[") {
      const closeBracket = findClosingBracket(src, pos + 1);
      if (closeBracket !== -1 && src[closeBracket + 1] === "(") {
        const parsed = parseLinkDestAndTitle(src, closeBracket + 1);
        if (parsed) {
          const alt = src.slice(pos + 2, closeBracket);
          const safeHref = sanitizeLinkHref(parsed.dest);
          if (safeHref && safeHref.startsWith("/")) {
            tokens.push({ type: "image", src: safeHref, alt, title: parsed.title });
          } else if (safeHref) {
            tokens.push({
              type: "link",
              href: safeHref,
              title: parsed.title,
              children: [{ type: "text", text: alt || safeHref }],
            });
          } else {
            pushText(tokens, src.slice(pos, parsed.endParenIdx + 1));
          }
          pos = parsed.endParenIdx + 1;
          continue;
        }
      }
    }

    if (ch === "[" && !insideLink) {
      const closeBracket = findClosingBracket(src, pos);
      if (closeBracket !== -1 && src[closeBracket + 1] === "(") {
        const parsed = parseLinkDestAndTitle(src, closeBracket + 1);
        if (parsed) {
          const label = src.slice(pos + 1, closeBracket);
          const safeHref = sanitizeLinkHref(parsed.dest);
          if (safeHref) {
            tokens.push({
              type: "link",
              href: safeHref,
              title: parsed.title,
              children: tokenizeInlineMarkdown(label, { depth: depth + 1, insideLink: true }),
            });
          } else {
            pushText(tokens, src.slice(pos, parsed.endParenIdx + 1));
          }
          pos = parsed.endParenIdx + 1;
          continue;
        }
      }
    }

    if (ch === "<" && !insideLink) {
      const tail = src.slice(pos);
      const urlAuto = /^<(https?:\/\/[^\s<>]+|mailto:[^\s<>]+)>/i.exec(tail);
      if (urlAuto) {
        const safeHref = sanitizeLinkHref(urlAuto[1]);
        if (safeHref) {
          tokens.push({
            type: "link",
            href: safeHref,
            title: "",
            children: [{ type: "text", text: urlAuto[1] }],
          });
          pos += urlAuto[0].length;
          continue;
        }
      }
      const mailAuto = /^<([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})>/.exec(tail);
      if (mailAuto) {
        const safeHref = sanitizeLinkHref(`mailto:${mailAuto[1]}`);
        if (safeHref) {
          tokens.push({
            type: "link",
            href: safeHref,
            title: "",
            children: [{ type: "text", text: mailAuto[1] }],
          });
          pos += mailAuto[0].length;
          continue;
        }
      }
    }

    if (!insideLink && (ch === "h" || ch === "H")) {
      const prev = pos > 0 ? src[pos - 1] : "";
      if (!/[a-zA-Z0-9]/.test(prev)) {
        const m = /^https?:\/\/[^\s<>"'`]+/i.exec(src.slice(pos));
        if (m) {
          const candidate = trimTrailingURLPunctuation(m[0]);
          const afterScheme = candidate.replace(/^https?:\/\//i, "");
          if (afterScheme.length > 0 && /[a-zA-Z0-9]/.test(afterScheme[0])) {
            const safeHref = sanitizeLinkHref(candidate);
            if (safeHref) {
              tokens.push({
                type: "link",
                href: safeHref,
                title: "",
                children: [{ type: "text", text: candidate }],
              });
              pos += candidate.length;
              continue;
            }
          }
        }
      }
    }

    if (!insideLink && (ch === "w" || ch === "W")) {
      const prev = pos > 0 ? src[pos - 1] : "";
      if (!prev || !/[a-zA-Z0-9./@]/.test(prev)) {
        const m = /^www\.[a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)+(?:[/?#][^\s<>"'`]*)?/i.exec(src.slice(pos));
        if (m) {
          const candidate = trimTrailingURLPunctuation(m[0]);
          const safeHref = sanitizeLinkHref(`https://${candidate}`);
          if (safeHref) {
            tokens.push({
              type: "link",
              href: safeHref,
              title: "",
              children: [{ type: "text", text: candidate }],
            });
            pos += candidate.length;
            continue;
          }
        }
      }
    }

    let matchedDelim = false;
    for (const delim of DELIMITERS) {
      if (!src.startsWith(delim, pos)) {
        continue;
      }
      const prev = pos > 0 ? src[pos - 1] : "";
      const next = pos + delim.length < src.length ? src[pos + delim.length] : "";
      if (isWhitespace(next)) {
        continue;
      }
      if (delim[0] === "_" && isWordChar(prev)) {
        continue;
      }
      const closeIdx = findClosingDelimiter(src, pos + delim.length, delim);
      if (closeIdx === -1) {
        continue;
      }
      const inner = src.slice(pos + delim.length, closeIdx);
      const children = tokenizeInlineMarkdown(inner, { depth: depth + 1, insideLink });
      if (delim === "***" || delim === "___") {
        tokens.push({ type: "strong", children: [{ type: "em", children }] });
      } else if (delim === "**" || delim === "__") {
        tokens.push({ type: "strong", children });
      } else if (delim === "~~") {
        tokens.push({ type: "del", children });
      } else {
        tokens.push({ type: "em", children });
      }
      pos = closeIdx + delim.length;
      matchedDelim = true;
      break;
    }
    if (matchedDelim) {
      continue;
    }

    pushText(tokens, ch);
    pos += 1;
  }

  return tokens;
}

const FENCE_RE = /^ {0,3}(`{3,}|~{3,})([^\n]*)$/;
const HEADING_RE = /^ {0,3}(#{1,6})(?:[ \t]+(.*)|[ \t]*)$/;
const HR_RE = /^ {0,3}(?:(\*[ \t]*){3,}|(-[ \t]*){3,}|(_[ \t]*){3,})$/;
const BLOCKQUOTE_RE = /^ {0,3}> ?(.*)$/;
const UL_RE = /^( *)([-*+])(?:[ \t]+(.*)|[ \t]*)$/;
const OL_RE = /^( *)(\d{1,9})([.)])(?:[ \t]+(.*)|[ \t]*)$/;
const TASK_RE = /^\[([ xX])\](?:[ \t]+(.*)|[ \t]*)$/;

function matchFenceOpen(line) {
  const m = FENCE_RE.exec(line);
  if (!m) {
    return null;
  }
  const fence = m[1];
  const info = m[2] || "";
  if (fence[0] === "`" && info.includes("`")) {
    return null;
  }
  const lang = info.trim().split(/\s+/)[0] || "";
  return { char: fence[0], len: fence.length, lang };
}

function splitTableRow(line) {
  let s = line.trim();
  if (s.startsWith("|")) {
    s = s.slice(1);
  }
  if (s.endsWith("|") && !s.endsWith("\\|")) {
    s = s.slice(0, -1);
  }
  const cells = [];
  let cur = "";
  let inCode = 0;
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (ch === "\\" && i + 1 < s.length) {
      if (s[i + 1] === "|") {
        cur += "|";
        i += 1;
        continue;
      }
      cur += ch + s[i + 1];
      i += 1;
      continue;
    }
    if (ch === "`") {
      let run = 1;
      while (i + run < s.length && s[i + run] === "`") {
        run += 1;
      }
      if (inCode === 0) {
        inCode = run;
      } else if (inCode === run) {
        inCode = 0;
      }
      cur += s.slice(i, i + run);
      i += run - 1;
      continue;
    }
    if (ch === "|" && inCode === 0) {
      cells.push(cur.trim());
      cur = "";
      continue;
    }
    cur += ch;
  }
  cells.push(cur.trim());
  return cells;
}

function parseTableDelimiter(line) {
  if (!line || !line.includes("|")) {
    return null;
  }
  const cells = splitTableRow(line);
  if (cells.length === 0) {
    return null;
  }
  const alignments = [];
  for (const c of cells) {
    if (!/^:?-{1,}:?$/.test(c)) {
      return null;
    }
    const left = c.startsWith(":");
    const right = c.endsWith(":");
    if (left && right) {
      alignments.push("center");
    } else if (left) {
      alignments.push("left");
    } else if (right) {
      alignments.push("right");
    } else {
      alignments.push(null);
    }
  }
  return alignments;
}

function matchListItem(line) {
  const expanded = expandTabs(line);
  const ul = UL_RE.exec(expanded);
  if (ul && !HR_RE.test(expanded)) {
    const indent = ul[1].length;
    const markerLen = ul[2].length + 1;
    return {
      ordered: false,
      marker: ul[2],
      start: 1,
      indent,
      contentIndent: indent + markerLen,
      text: ul[3] || "",
    };
  }
  const ol = OL_RE.exec(expanded);
  if (ol) {
    const indent = ol[1].length;
    const markerLen = ol[2].length + ol[3].length + 1;
    return {
      ordered: true,
      marker: ol[3],
      start: Number.parseInt(ol[2], 10) || 1,
      indent,
      contentIndent: indent + markerLen,
      text: ol[4] || "",
    };
  }
  return null;
}

function isTableStart(lines, idx) {
  if (idx + 1 >= lines.length) {
    return false;
  }
  const hdrLine = lines[idx];
  if (!hdrLine.includes("|")) {
    return false;
  }
  const alignments = parseTableDelimiter(lines[idx + 1]);
  if (!alignments) {
    return false;
  }
  const hdrCells = splitTableRow(hdrLine);
  return hdrCells.length === alignments.length;
}

function isBlockStarter(lines, idx) {
  const line = lines[idx];
  if (!line || line.trim() === "") {
    return true;
  }
  if (matchFenceOpen(line)) {
    return true;
  }
  if (HEADING_RE.test(line)) {
    return true;
  }
  if (HR_RE.test(line)) {
    return true;
  }
  if (BLOCKQUOTE_RE.test(line)) {
    return true;
  }
  if (matchListItem(line)) {
    return true;
  }
  if (isTableStart(lines, idx)) {
    return true;
  }
  return false;
}

function cleanParagraphLineBreak(line) {
  if (line.endsWith("\\")) {
    return line.slice(0, -1);
  }
  return line.replace(/[ \t]{2,}$/, "");
}

function parseListBlock(lines, startIdx) {
  const first = matchListItem(lines[startIdx]);
  const baseIndent = first.indent;
  const ordered = first.ordered;
  const marker = first.marker;
  const start = first.start;
  const items = [];
  let i = startIdx;

  while (i < lines.length) {
    const cur = matchListItem(lines[i]);
    if (!cur || cur.ordered !== ordered || cur.marker !== marker || cur.indent > baseIndent + 3 || cur.indent < baseIndent) {
      break;
    }

    let firstText = cur.text;
    let checked = null;
    const taskMatch = TASK_RE.exec(firstText);
    if (taskMatch) {
      checked = taskMatch[1].toLowerCase() === "x";
      firstText = taskMatch[2] || "";
    }

    const bodyLines = [firstText];
    const minContIndent = Math.max(cur.contentIndent, baseIndent + 2);
    i += 1;

    while (i < lines.length) {
      const rawLine = lines[i];
      if (rawLine.trim() === "") {
        let look = i + 1;
        while (look < lines.length && lines[look].trim() === "") {
          look += 1;
        }
        if (look >= lines.length) {
          i = look;
          break;
        }
        const nextLine = lines[look];
        const nextInd = leadingIndent(nextLine);
        const nextItem = matchListItem(nextLine);
        if (nextItem && nextItem.ordered === ordered && nextItem.marker === marker && nextItem.indent <= baseIndent + 3 && nextItem.indent >= baseIndent) {
          i = look;
          break;
        }
        if (nextInd >= minContIndent) {
          bodyLines.push("");
          i += 1;
          continue;
        }
        i = look;
        break;
      }

      const ind = leadingIndent(rawLine);
      const nextItem = matchListItem(rawLine);
      if (nextItem) {
        if (nextItem.indent >= minContIndent || nextItem.indent > baseIndent + 1) {
          bodyLines.push(stripIndent(expandTabs(rawLine), minContIndent));
          i += 1;
          continue;
        }
        break;
      }

      if (ind >= minContIndent) {
        bodyLines.push(stripIndent(expandTabs(rawLine), minContIndent));
        i += 1;
        continue;
      }

      if (isBlockStarter(lines, i)) {
        break;
      }

      const prevLine = bodyLines[bodyLines.length - 1];
      if (prevLine === "") {
        break;
      }
      bodyLines.push(rawLine.trimStart());
      i += 1;
    }

    const innerBlocks = parseMarkdownBlocks(bodyLines.join("\n"));
    let inline = [];
    let blocks = innerBlocks;
    if (innerBlocks.length > 0 && innerBlocks[0].type === "paragraph") {
      inline = innerBlocks[0].children;
      blocks = innerBlocks.slice(1);
    }
    items.push({ checked, inline, blocks });
  }

  return {
    block: { type: "list", ordered, start, items },
    nextIdx: i,
  };
}

/**
 * Parse markdown text into block AST nodes.
 */
export function parseMarkdownBlocks(raw) {
  const text = String(raw || "").replace(/\r\n?/g, "\n");
  if (!text) {
    return [];
  }
  const lines = text.split("\n");
  const blocks = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];
    if (line.trim() === "") {
      i += 1;
      continue;
    }

    const fence = matchFenceOpen(line);
    if (fence) {
      const codeLines = [];
      i += 1;
      while (i < lines.length) {
        const closeLine = lines[i];
        const m = /^ {0,3}(`{3,}|~{3,})[ \t]*$/.exec(closeLine);
        if (m && m[1][0] === fence.char && m[1].length >= fence.len) {
          i += 1;
          break;
        }
        codeLines.push(closeLine);
        i += 1;
      }
      blocks.push({ type: "code", lang: fence.lang, code: codeLines.join("\n") });
      continue;
    }

    const heading = HEADING_RE.exec(line);
    if (heading) {
      const level = heading[1].length;
      const headingText = (heading[2] || "").replace(/[ \t]+#+[ \t]*$/, "").replace(/^#+[ \t]*$/, "");
      blocks.push({ type: "heading", level, children: tokenizeInlineMarkdown(headingText) });
      i += 1;
      continue;
    }

    if (HR_RE.test(line)) {
      blocks.push({ type: "hr" });
      i += 1;
      continue;
    }

    if (BLOCKQUOTE_RE.test(line)) {
      const quoteLines = [];
      while (i < lines.length) {
        const qm = BLOCKQUOTE_RE.exec(lines[i]);
        if (!qm) {
          break;
        }
        quoteLines.push(qm[1]);
        i += 1;
      }
      blocks.push({ type: "blockquote", children: parseMarkdownBlocks(quoteLines.join("\n")) });
      continue;
    }

    if (isTableStart(lines, i)) {
      const alignments = parseTableDelimiter(lines[i + 1]);
      const colCount = alignments.length;
      const normalizeCells = (rawCells) => {
        const out = [];
        for (let c = 0; c < colCount; c++) {
          out.push(tokenizeInlineMarkdown(rawCells[c] || ""));
        }
        return out;
      };
      const header = normalizeCells(splitTableRow(lines[i]));
      const rows = [];
      i += 2;
      while (i < lines.length) {
        const rowLine = lines[i];
        if (!rowLine || rowLine.trim() === "" || !rowLine.includes("|")) {
          break;
        }
        if (matchFenceOpen(rowLine) || HEADING_RE.test(rowLine) || HR_RE.test(rowLine) || BLOCKQUOTE_RE.test(rowLine) || matchListItem(rowLine)) {
          break;
        }
        rows.push(normalizeCells(splitTableRow(rowLine)));
        i += 1;
      }
      blocks.push({ type: "table", alignments, header, rows });
      continue;
    }

    if (matchListItem(line)) {
      const { block, nextIdx } = parseListBlock(lines, i);
      blocks.push(block);
      i = nextIdx > i ? nextIdx : i + 1;
      continue;
    }

    const paraLines = [];
    while (i < lines.length) {
      if (lines[i].trim() === "") {
        break;
      }
      if (paraLines.length > 0 && isBlockStarter(lines, i)) {
        break;
      }
      paraLines.push(cleanParagraphLineBreak(lines[i]));
      i += 1;
    }
    blocks.push({ type: "paragraph", children: tokenizeInlineMarkdown(paraLines.join("\n")) });
  }

  return blocks;
}

function renderInlineNodes(tokens) {
  const frag = document.createDocumentFragment();
  for (const tok of tokens || []) {
    if (!tok) {
      continue;
    }
    switch (tok.type) {
      case "text":
        frag.appendChild(document.createTextNode(tok.text));
        break;
      case "br":
        frag.appendChild(el("br"));
        break;
      case "code":
        frag.appendChild(el("code", { className: "inline-code", text: tok.text }));
        break;
      case "strong":
        frag.appendChild(el("strong", null, renderInlineNodes(tok.children)));
        break;
      case "em":
        frag.appendChild(el("em", null, renderInlineNodes(tok.children)));
        break;
      case "del":
        frag.appendChild(el("del", null, renderInlineNodes(tok.children)));
        break;
      case "link": {
        const props = {
          className: "chat-link",
          href: tok.href,
          title: tok.title || null,
        };
        if (!tok.href.startsWith("#")) {
          props.target = "_blank";
          props.rel = "noopener noreferrer";
        }
        frag.appendChild(el("a", props, renderInlineNodes(tok.children)));
        break;
      }
      case "image":
        frag.appendChild(
          el("img", {
            className: "chat-md-img",
            src: tok.src,
            alt: tok.alt || "",
            title: tok.title || null,
            loading: "lazy",
          }),
        );
        break;
      default:
        break;
    }
  }
  return frag;
}

export function renderInlineDOM(text) {
  return renderInlineNodes(tokenizeInlineMarkdown(text));
}

function renderBlocksInto(container, blocks) {
  for (const block of blocks) {
    switch (block.type) {
      case "heading": {
        const tag = `h${Math.min(Math.max(block.level || 1, 1), 6)}`;
        container.appendChild(el(tag, { className: `chat-h chat-h${block.level}` }, renderInlineNodes(block.children)));
        break;
      }
      case "paragraph":
        container.appendChild(el("p", { className: "chat-p" }, renderInlineNodes(block.children)));
        break;
      case "hr":
        container.appendChild(el("hr", { className: "chat-hr" }));
        break;
      case "blockquote": {
        const bq = el("blockquote", { className: "chat-quote" });
        renderBlocksInto(bq, block.children);
        container.appendChild(bq);
        break;
      }
      case "code": {
        const copyBtn = el("button", {
          type: "button",
          className: "chat-code-copy",
          text: "Copy",
          onclick: (ev) => {
            ev.stopPropagation();
            copyText(block.code, copyBtn);
          },
        });
        const header = el(
          "div",
          { className: "chat-code-header" },
          el("span", { className: "chat-code-lang", text: block.lang || "code" }),
          copyBtn,
        );
        const pre = el("pre", { className: "code-block" }, el("code", { text: block.code }));
        container.appendChild(el("div", { className: "chat-code-wrap" }, header, pre));
        break;
      }
      case "list": {
        const tag = block.ordered ? "ol" : "ul";
        const props = { className: block.ordered ? "chat-list chat-ol" : "chat-list chat-ul" };
        if (block.ordered && block.start && block.start !== 1) {
          props.start = String(block.start);
        }
        const listEl = el(tag, props);
        for (const item of block.items) {
          const isTask = item.checked !== null && item.checked !== undefined;
          const li = el("li", isTask ? { className: "chat-task-item" } : null);
          if (isTask) {
            li.appendChild(
              el("input", {
                type: "checkbox",
                className: "chat-task-check",
                checked: Boolean(item.checked),
                disabled: true,
                "aria-label": item.checked ? "Completed task" : "Pending task",
              }),
            );
          }
          if (item.inline && item.inline.length > 0) {
            li.appendChild(renderInlineNodes(item.inline));
          }
          if (item.blocks && item.blocks.length > 0) {
            renderBlocksInto(li, item.blocks);
          }
          listEl.appendChild(li);
        }
        container.appendChild(listEl);
        break;
      }
      case "table": {
        const alignClass = (idx) => {
          const a = block.alignments[idx];
          return a ? `chat-align-${a}` : null;
        };
        const theadRow = el("tr");
        block.header.forEach((cellTokens, idx) => {
          theadRow.appendChild(el("th", { className: alignClass(idx) }, renderInlineNodes(cellTokens)));
        });
        const tbody = el("tbody");
        for (const row of block.rows) {
          const tr = el("tr");
          row.forEach((cellTokens, idx) => {
            tr.appendChild(el("td", { className: alignClass(idx) }, renderInlineNodes(cellTokens)));
          });
          tbody.appendChild(tr);
        }
        container.appendChild(
          el(
            "div",
            { className: "chat-table-wrap" },
            el("table", { className: "chat-table" }, el("thead", null, theadRow), tbody),
          ),
        );
        break;
      }
      default:
        break;
    }
  }
}

export function renderMarkdownDOM(raw) {
  const wrap = el("div", { className: "chat-md" });
  const blocks = parseMarkdownBlocks(raw);
  if (blocks.length > 0) {
    renderBlocksInto(wrap, blocks);
  }
  return wrap;
}
