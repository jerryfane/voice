// OMP v18.2.11: context runs before each model call and returns { messages }.
// https://github.com/can1357/oh-my-pi/blob/v18.2.11/packages/coding-agent/src/extensibility/extensions/runner.ts
// Keep this transform immutable: emitContext's structuredClone can fall back to
// a shallow array copy. Never change persisted messages or remove tool pairs.
import path from "node:path";

type RecordValue = Record<string, unknown>;
type Observation = {
  phone: string;
  messageIndex: number;
  blockIndex: number;
  start: number;
  end: number;
  screenshot?: string;
};
type Call = { name: string; args: RecordValue; observation?: Observation };

function record(value: unknown): value is RecordValue {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

// Deliberately not a shell interpreter. Only literal argv and && are accepted;
// expansion, redirection, pipelines, comments, scripts and ambiguous syntax
// remain unpruned. Quoted mentions of an observe command are not commands.
function literalCommands(command: string): string[][] | undefined {
  const commands: string[][] = [];
  let words: string[] = [];
  let word = "";
  let started = false;
  let quote = "";
  const finishWord = () => {
    if (started) words.push(word);
    word = "";
    started = false;
  };
  for (let i = 0; i < command.length; i += 1) {
    const char = command[i];
    if (quote === "'") {
      if (char === quote) quote = "";
      else word += char;
      continue;
    }
    if (char === "\\") {
      const next = command[++i];
      if (next === undefined || next === "\n" || next === "\r") return;
      // POSIX double quotes only consume these escapes.
      if (quote && !'$`"\\'.includes(next)) word += "\\";
      word += next;
      started = true;
      continue;
    }
    if (char === "$" || char === "`") return;
    if (quote === '"') {
      if (char === quote) quote = "";
      else word += char;
      continue;
    }
    if (char === "'" || char === '"') {
      quote = char;
      started = true;
    } else if (char === " " || char === "\t") {
      finishWord();
    } else if (char === "&" && command[i + 1] === "&") {
      finishWord();
      if (!words.length) return;
      commands.push(words);
      words = [];
      i += 1;
    } else if (/[\n\r;&|<>(){}*?\[\]#!~]/.test(char)) {
      return;
    } else {
      word += char;
      started = true;
    }
  }
  if (quote) return;
  finishWord();
  if (!words.length) return;
  commands.push(words);
  return commands;
}

function phoneArgs(words: string[]): string[] | undefined {
  if (words[0] !== "voice" && words[0] !== "/usr/local/bin/voice") return;
  let offset = 1;
  if (words[offset] === "--config" && words[offset + 1]) offset += 2;
  else if (words[offset]?.startsWith("--config=")) offset += 1;
  if (words[offset] !== "phone" || !words[offset + 1]) return;
  return words.slice(offset + 1);
}

function observedPhone(command: unknown): string | undefined {
  if (typeof command !== "string") return;
  const commands = literalCommands(command);
  if (!commands) return;
  // Prefixes may produce arbitrary ADB output: preserve them verbatim. No
  // shell/env manipulation or other producers may masquerade as Voice here.
  for (const words of commands.slice(0, -1)) {
    const args = phoneArgs(words);
    if (args?.[1] === "adb" && args.length > 2) continue;
    if (words.length === 2 && words[0] === "sleep" && /^\d+(?:\.\d+)?$/.test(words[1])) continue;
    return;
  }
  const args = phoneArgs(commands[commands.length - 1]);
  if (!args || args[1] !== "observe") return;
  if (args.length !== 2 && !(args.length === 3 && ["--screenshot", "--both"].includes(args[2]))) return;
  return args[0];
}

function screenshotPath(value: unknown): string | undefined {
  if (typeof value !== "string" || !value.startsWith("/") || !value.endsWith(".png")) return;
  if (/[\x00-\x1f;:?]/.test(value)) return;
  return path.posix.normalize(value);
}

function observationSpan(text: string): { start: number; end: number; payload: RecordValue } | undefined {
  // bash.ts appends formatWallTimeNotice after stdout, not a JSON envelope.
  // Only a complete final JSON object is eligible, never JSON mentioned in UI
  // text, a recursively discovered field, or a truncated/artifact preview.
  const timing = /\n\nWall time: [0-9.]+ seconds\s*$/.exec(text);
  const end = (timing ? text.slice(0, timing.index) : text).trimEnd().length;
  if (text[end - 1] !== "}") return;
  let start = text.lastIndexOf("\n", end - 1) + 1;
  while (start >= 0) {
    const candidate = text.slice(start, end).trimStart();
    if (candidate.startsWith("{")) {
      try {
        const payload: unknown = JSON.parse(candidate);
        if (!record(payload)) return;
        const marked = payload.kind === "voice.android.observation";
        const legacy = payload.kind === undefined && typeof payload.ui_hierarchy === "string";
        if (!marked && !legacy) return;
        if (marked && !record(payload.ui) && !screenshotPath(payload.screenshot_path)) return;
        return { start: start + text.slice(start, end).length - candidate.length, end, payload };
      } catch {
        // A pretty-printed root may start on an earlier line.
      }
    }
    if (start === 0) return;
    start = start > 1 ? text.lastIndexOf("\n", start - 2) + 1 : 0;
  }
}

function truncatedObservation(text: string): { start: number; end: number; payload: RecordValue } | undefined {
  // Observed in the persistent v18.2.11 session: native per-line truncation
  // breaks the old single-line JSON. Accept only a standalone observe command
  // (checked by the caller), a root schema prefix, and the native footer.
  if (!/\n\[Some lines truncated to \d+ bytes\. Read artifact:\/\/\d+ for full output\]\s*$/.test(text)) return;
  const legacy = /^\s*\{\s*"screenshot_path"\s*:\s*("(?:\\.|[^"\\])*")\s*,\s*"ui_hierarchy"\s*:\s*"/.exec(text);
  const marked = /^\s*\{\s*"kind"\s*:\s*"voice\.android\.observation"\s*,\s*"(?:ui|screenshot_path)"\s*:/.test(text);
  if (!legacy && !marked) return;
  const payload: RecordValue = {};
  if (legacy) {
    try {
      payload.screenshot_path = JSON.parse(legacy[1]);
    } catch {
      return;
    }
  }
  return { start: 0, end: text.length, payload };
}

function successful(message: RecordValue): boolean {
  if (message.isError === true) return false;
  if (!record(message.details)) return true;
  return message.details.timedOut !== true &&
    (message.details.exitCode === undefined || message.details.exitCode === 0);
}

/** Return model-only replacements; all input objects and tool IDs stay intact. */
export function prunePhoneContext(messages: unknown[]): unknown[] {
  const calls = new Map<string, Call>();
  const duplicateIds = new Set<string>();
  const observations: Observation[] = [];
  const latest = new Map<string, Observation>();
  const imageReads: { index: number; observation: Observation }[] = [];

  // Reject ambiguous reused IDs before considering their results.
  const ids = new Set<string>();
  for (const message of messages) {
    if (!record(message) || message.role !== "assistant" || !Array.isArray(message.content)) continue;
    for (const block of message.content) {
      if (!record(block) || block.type !== "toolCall" || typeof block.id !== "string") continue;
      if (ids.has(block.id)) duplicateIds.add(block.id);
      ids.add(block.id);
    }
  }

  for (let index = 0; index < messages.length; index += 1) {
    const message = messages[index];
    if (!record(message) || !Array.isArray(message.content)) continue;
    if (message.role === "assistant") {
      let capturePending = message.content.some(block => record(block) && block.type === "toolCall" &&
        block.name === "bash" && record(block.arguments) && observedPhone(block.arguments.command) !== undefined);
      for (const pending of calls.values()) {
        if (pending.name === "bash" && observedPhone(pending.args.command) !== undefined) capturePending = true;
      }
      for (const block of message.content) {
        if (!record(block) || block.type !== "toolCall" || typeof block.id !== "string" ||
            duplicateIds.has(block.id) || typeof block.name !== "string" || !record(block.arguments)) continue;
        const call: Call = { name: block.name, args: block.arguments };
        if (call.name === "read" && !capturePending) {
          const readPath = screenshotPath(call.args.path);
          if (readPath) {
            // A fixed snapshot filename can be reused. Bind at CALL time, not
            // result time, and never associate reads predating the observation.
            for (const observation of latest.values()) {
              if (observation.screenshot === readPath &&
                  (!call.observation || observation.messageIndex > call.observation.messageIndex)) {
                call.observation = observation;
              }
            }
          }
        }
        calls.set(block.id, call);
      }
      continue;
    }
    if (message.role !== "toolResult" || typeof message.toolCallId !== "string") continue;
    const call = calls.get(message.toolCallId);
    calls.delete(message.toolCallId);
    if (!call || message.toolName !== call.name || !successful(message)) continue;
    if (call.name === "read" && call.observation && latest.get(call.observation.phone) === call.observation &&
        message.content.some(block => record(block) && block.type === "image")) {
      imageReads.push({ index, observation: call.observation });
      continue;
    }
    if (call.name !== "bash") continue;
    const phone = observedPhone(call.args.command);
    if (!phone) continue;
    // The built-in bash result has one text block followed by optional terminal
    // images. Multiple text blocks are ambiguous; do not discard any of them.
    const textBlocks = message.content.filter(block => record(block) && block.type === "text");
    if (textBlocks.length !== 1 || typeof textBlocks[0].text !== "string") continue;
    const span = observationSpan(textBlocks[0].text) ??
      (literalCommands(call.args.command as string)?.length === 1 ? truncatedObservation(textBlocks[0].text) : undefined);
    if (!span) continue;
    const observation: Observation = {
      phone, messageIndex: index, blockIndex: message.content.indexOf(textBlocks[0]),
      start: span.start, end: span.end, screenshot: screenshotPath(span.payload.screenshot_path),
    };
    observations.push(observation);
    latest.set(phone, observation);
  }

  let output = messages;
  const replaceContent = (index: number, content: unknown[]) => {
    if (output === messages) output = messages.slice();
    output[index] = { ...(messages[index] as RecordValue), content };
  };
  for (const observation of observations) {
    if (latest.get(observation.phone) === observation) continue;
    const message = messages[observation.messageIndex] as RecordValue;
    const content = (message.content as RecordValue[]).slice();
    const block = content[observation.blockIndex];
    const text = block.text as string;
    content[observation.blockIndex] = {
      ...block,
      text: text.slice(0, observation.start) +
        "[Superseded phone observation omitted from model context; newer observation retained.]" +
        text.slice(observation.end),
    };
    replaceContent(observation.messageIndex, content);
  }
  for (const read of imageReads) {
    if (latest.get(read.observation.phone) === read.observation) continue;
    const message = messages[read.index] as RecordValue;
    replaceContent(read.index, (message.content as unknown[]).map(block =>
      record(block) && block.type === "image"
        ? { type: "text", text: "[Superseded phone screenshot omitted from model context.]" }
        : block));
  }
  return output;
}
