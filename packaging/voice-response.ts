import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";


const responseDir = process.env.VOICE_AGENT_RESPONSES;
const markerPath = process.env.VOICE_AGENT_MARKER;
const requestPattern = /<voice-request id="([A-Za-z0-9._-]{1,128})">/;
let currentSessionPath: string | undefined;
let speakNextAnswer = false;

type CommandContext = {
  ui: {
    notify(message: string, level: "info" | "warning" | "error"): void;
  };
};
type ExtensionAPI = {
  on(event: string, handler: (...args: unknown[]) => void): void;
  registerCommand(
    name: string,
    options: {
      description: string;
      handler(args: string, context: CommandContext): Promise<void> | void;
    },
  ): void;
  sendUserMessage(content: string): void;
};

function record(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object";
}
function report(state: "working" | "blocked" | "idle", message?: string): void {
  const pane = process.env.HERDR_PANE_ID;
  if (!pane || !currentSessionPath) return;
  const args = ["report", "state", pane, state, currentSessionPath];
  if (message) args.push(message);
  spawnSync("/usr/local/bin/herdr", args, {
    stdio: "ignore",
    timeout: 2_000,
  });
}
function reportSession(context: unknown, source: string): void {
  const pane = process.env.HERDR_PANE_ID;
  if (!pane || !record(context) || !record(context.sessionManager)) return;
  const getter = context.sessionManager.getSessionFile;
  if (typeof getter !== "function") return;
  const session = Reflect.apply(getter, context.sessionManager, []);
  if (typeof session !== "string" || !path.isAbsolute(session)) return;
  currentSessionPath = session;
  spawnSync("/usr/local/bin/herdr", ["report", "session", pane, session, source], {
    stdio: "ignore",
    timeout: 2_000,
  });
}




function text(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((block: unknown) => {
      if (typeof block === "string") return block;
      if (record(block) && block.type === "text" && typeof block.text === "string") {
        return block.text;
      }
      return "";
    })
    .filter(Boolean)
    .join("\n");
}

function lastMessage(messages: unknown[], role: string): Record<string, unknown> | undefined {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const message = messages[i];
    if (record(message) && message.role === role) return message;
  }
  return undefined;
}

function publish(id: string, payload: Record<string, unknown>): void {
  if (!responseDir) return;
  fs.mkdirSync(responseDir, { recursive: true, mode: 0o700 });
  const destination = path.join(responseDir, `${id}.json`);
  const temporary = path.join(responseDir, `.${id}.${process.pid}.tmp`);
  fs.writeFileSync(temporary, `${JSON.stringify(payload)}\n`, { mode: 0o600 });
  fs.renameSync(temporary, destination);
}
function runVoiceCommand(args: string, context: CommandContext): void {
  const match = /^\s*say\s+([\s\S]*\S)\s*$/.exec(args);
  if (!match) {
    context.ui.notify("Usage: /voice say TEXT or /voice ask QUESTION", "warning");
    return;
  }
  const result = spawnSync(
    "/usr/local/bin/voice",
    ["--config", "/etc/voice/config.json", "say", match[1]],
    {
      encoding: "utf8",
      timeout: 60_000,
      env: { ...process.env, HOME: "/home/voice-agent" },
    },
  );
  if (result.error || result.status !== 0) {
    let detail = result.error?.message ?? "";
    if (!detail && typeof result.stderr === "string") {
      detail = result.stderr.trim();
    }
    context.ui.notify(detail || "Voice playback failed", "error");
    return;
  }
  context.ui.notify("Spoken.", "info");
}

function runVoicePlan(output: string, context: CommandContext): void {
  let plan = output;
  try {
    const parsed: unknown = JSON.parse(output);
    if (!record(parsed)) throw new Error("plan is not an object");
    plan = JSON.stringify(parsed);
  } catch {
    plan = JSON.stringify({ speak: output, actions: [] });
  }
  const result = spawnSync(
    "/usr/local/bin/voice",
    ["--config", "/etc/voice/config.json", "execute", plan],
    {
      encoding: "utf8",
      timeout: 120_000,
      env: { ...process.env, HOME: "/home/voice-agent" },
    },
  );
  if (result.error || result.status !== 0) {
    let detail = result.error?.message ?? "";
    if (!detail && typeof result.stderr === "string") {
      detail = result.stderr.trim();
    }
    context.ui.notify(detail || "Voice plan failed", "error");
    return;
  }
  context.ui.notify("Voice plan completed.", "info");
}


export default function (pi: ExtensionAPI) {
  pi.registerCommand("voice", {
    description: "Speak text or ask for a spoken answer (/voice say|ask ...)",
    handler: async (args, context) => {
      const ask = /^\s*ask\s+([\s\S]*\S)\s*$/.exec(args);
      if (!ask) {
        runVoiceCommand(args, context);
        return;
      }
      if (speakNextAnswer) {
        context.ui.notify("A spoken Voice answer is already pending.", "warning");
        return;
      }
      speakNextAnswer = true;
      context.ui.notify("Asking Voice…", "info");
      pi.sendUserMessage(ask[1]);
    },
  });


  pi.on("session_start", (_event: unknown, context: unknown) => {
    if (markerPath) {
      fs.mkdirSync(path.dirname(markerPath), { recursive: true, mode: 0o700 });
      fs.closeSync(fs.openSync(markerPath, "a", 0o600));
    }
    reportSession(context, "startup");
    report("idle");
  });

  pi.on("session_switch", (_event: unknown, context: unknown) => {
    reportSession(context, "resume");
    report("idle");
  });
  pi.on("agent_start", () => report("working"));
  pi.on("tool_approval_requested", () => report("blocked", "tool approval required"));
  pi.on("tool_approval_resolved", () => report("working"));

  pi.on("agent_end", (event: unknown, context: unknown) => {
    const messages =
      record(event) && Array.isArray(event.messages) ? event.messages : [];
    const user = lastMessage(messages, "user");
    const assistant = lastMessage(messages, "assistant");
    const output = text(assistant?.content).trim();
    const match = requestPattern.exec(text(user?.content));
    if (match) {
      if (assistant?.stopReason === "error") {
        publish(match[1], {
          ok: false,
          error:
            typeof assistant.errorMessage === "string"
              ? assistant.errorMessage
              : "OMP provider error",
        });
      } else if (!output) {
        publish(match[1], { ok: false, error: "OMP returned no final response" });
      } else {
        publish(match[1], { ok: true, output });
      }
    }
    if (speakNextAnswer) {
      speakNextAnswer = false;
      const commandContext = context as CommandContext;
      if (assistant?.stopReason === "error") {
        commandContext.ui.notify(
          typeof assistant.errorMessage === "string"
            ? assistant.errorMessage
            : "Voice agent failed",
          "error",
        );
      } else if (!output) {
        commandContext.ui.notify("Voice agent returned no answer.", "error");
      } else {
        runVoicePlan(output, commandContext);
      }
    }
    report("idle");
  });
}
