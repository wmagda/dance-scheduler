#!/usr/bin/env node
/**
 * Lightweight WhatsApp Bridge for Thursday Latin Dancing Scheduler.
 *
 * Uses @whiskeysockets/baileys (pure WebSocket protocol — no Chromium needed on RPi).
 *
 * 1. Outbound webhook (`POST http://127.0.0.1:8096/send`):
 *    Called by Go server (`TRANSPORT_MODE=whatsapp`, `BOT_WEBHOOK_URL=http://127.0.0.1:8096/send`)
 *    for `dm`, `otp`, and `group` messages.
 * 2. Inbound slash commands (`/claim`, `/token`, `/lead`, `/follow`, `/status`, `/help`):
 *    Forwards to `POST http://127.0.0.1:8095/api/bot/inbound` and replies with the result.
 * 3. Helper endpoint (`GET http://127.0.0.1:8096/groups`):
 *    Lists all WhatsApp groups the bot account has joined + their `@g.us` JIDs.
 */

import http from "node:http";
import makeWASocket, {
  DisconnectReason,
  useMultiFileAuthState,
} from "@whiskeysockets/baileys";
import pino from "pino";
import qrcode from "qrcode-terminal";

const PORT = Number(process.env.BOT_PORT || 8096);
const SCHEDULER_URL = process.env.SCHEDULER_URL || "http://127.0.0.1:8095";
const GROUP_JID = process.env.WHATSAPP_GROUP_JID || ""; // e.g. "120363012345678901@g.us"
const AUTH_DIR = process.env.WHATSAPP_AUTH_DIR || "./auth_info";

let sock = null;

function phoneToJid(phone) {
  const digits = String(phone || "").replace(/[^0-9]/g, "");
  if (!digits) return "";
  const normalized = digits.length === 10 ? "1" + digits : digits;
  return `${normalized}@s.whatsapp.net`;
}

function jidToPhone(jid) {
  const user = String(jid || "").split("@")[0].split(":")[0];
  return "+" + user.replace(/[^0-9]/g, "");
}

async function startWhatsApp() {
  const { state, saveCreds } = await useMultiFileAuthState(AUTH_DIR);
  sock = makeWASocket({
    auth: state,
    logger: pino({ level: "silent" }),
    printQRInTerminal: false,
  });

  sock.ev.on("creds.update", saveCreds);

  sock.ev.on("connection.update", (update) => {
    const { connection, lastDisconnect, qr } = update;
    if (qr) {
      console.log("\n=== Scan this QR code in WhatsApp (Linked Devices -> Link a Device) ===");
      qrcode.generate(qr, { small: true });
    }
    if (connection === "open") {
      console.log("WhatsApp connected! Listening for commands & webhook sends on port", PORT);
      if (!GROUP_JID) {
        console.log("TIP: Visit http://127.0.0.1:" + PORT + "/groups to find your instructors group JID.");
      }
    }
    if (connection === "close") {
      const code = lastDisconnect?.error?.output?.statusCode;
      const shouldReconnect = code !== DisconnectReason.loggedOut;
      console.log("WhatsApp connection closed (code=" + code + "). Reconnecting:", shouldReconnect);
      if (shouldReconnect) {
        setTimeout(startWhatsApp, 4000);
      }
    }
  });

  sock.ev.on("messages.upsert", async ({ messages, type }) => {
    if (type !== "notify") return;
    for (const msg of messages) {
      if (!msg.message || msg.key.fromMe) continue;
      const text =
        msg.message.conversation ||
        msg.message.extendedTextMessage?.text ||
        "";
      if (!text.trim().startsWith("/")) continue;

      const senderJid = msg.key.participant || msg.key.remoteJid;
      const phone = jidToPhone(senderJid);

      try {
        const res = await fetch(`${SCHEDULER_URL}/api/bot/inbound`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ phone, text: text.trim() }),
        });
        const data = await res.json();
        const reply = data.reply || data.error;
        if (reply) {
          // Always reply via DM to keep group noise low.
          const dmJid = phoneToJid(phone);
          await sock.sendMessage(dmJid, { text: reply });
        }
      } catch (err) {
        console.error("Inbound forward error:", err.message);
      }
    }
  });
}

const server = http.createServer(async (req, res) => {
  if (req.method === "GET" && req.url === "/groups") {
    if (!sock) {
      res.writeHead(503, { "Content-Type": "application/json" });
      return res.end(JSON.stringify({ error: "WhatsApp not connected" }));
    }
    try {
      const groups = await sock.groupFetchAllParticipating();
      const summary = Object.values(groups).map((g) => ({
        jid: g.id,
        subject: g.subject,
        size: g.participants?.length || 0,
      }));
      res.writeHead(200, { "Content-Type": "application/json" });
      return res.end(JSON.stringify(summary, null, 2));
    } catch (err) {
      res.writeHead(500, { "Content-Type": "application/json" });
      return res.end(JSON.stringify({ error: err.message }));
    }
  }

  if (req.method === "POST" && req.url === "/send") {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", async () => {
      try {
        const payload = JSON.parse(body);
        if (!sock) throw new Error("WhatsApp socket not ready");

        if (payload.channel === "otp") {
          const jid = phoneToJid(payload.phone);
          await sock.sendMessage(jid, { text: payload.text });
        } else if (payload.channel === "dm") {
          const jid = phoneToJid(payload.recipient?.phone);
          await sock.sendMessage(jid, { text: payload.text });
        } else if (payload.channel === "group") {
          if (!GROUP_JID) {
            console.warn("WHATSAPP_GROUP_JID not set; skipping group send:", payload.text);
          } else {
            await sock.sendMessage(GROUP_JID, { text: payload.text });
          }
        }
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ ok: true }));
      } catch (err) {
        console.error("Send error:", err.message);
        res.writeHead(500, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: err.message }));
      }
    });
    return;
  }

  res.writeHead(404);
  res.end("not found");
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`WhatsApp Bridge webhook listening on http://127.0.0.1:${PORT}`);
  startWhatsApp();
});
