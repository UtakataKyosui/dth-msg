"use strict";
let token = "";
let locked = false;
let clerkClient = null;
const el = id => document.getElementById(id);
function notice(message) { el("notice").textContent = message; el("notice").hidden = false; }
async function api(path, options = {}) {
  const bearer = clerkClient ? await clerkClient.session?.getToken() : token;
  const response = await fetch(path, { ...options, headers: { Authorization: "Bearer " + (bearer || ""), "Content-Type": "application/json" }, cache: "no-store", credentials: "same-origin" });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || "操作に失敗しました");
  return data;
}
let sequence = 0;
function addTarget(target = {}, message = "") {
  if (el("targets").children.length >= 10) return;
  const row = document.createElement("fieldset");
  const legend = document.createElement("legend"); legend.textContent = "送信先"; row.append(legend);
  const fields = {};
  for (const [key, title, value] of [["id", "送信先ID", target.id || ""], ["kind", "サービス", target.kind || "discord"], ["url", "Webhook / API URL", target.url || ""], ["token", "Mastodonアクセストークン", target.token || ""], ["message", "メッセージ（Discord / Slack: 2000文字、Mastodon: 500文字）", message]]) {
    const label = document.createElement("label"); label.textContent = title;
    const input = document.createElement(key === "kind" ? "select" : key === "message" ? "textarea" : "input");
    input.id = "field-" + sequence++; label.htmlFor = input.id;
    if (key === "kind") for (const kind of ["discord", "slack", "mastodon", ...(target.kind === "x" ? ["x"] : [])]) { const option = document.createElement("option"); option.value = kind; option.textContent = kind; input.append(option); }
    if (key === "url" || key === "token") { input.type = "password"; input.autocomplete = "off"; }
    input.required = ["id", "url"].includes(key);
    input.value = value; fields[key] = input; row.append(label, input);
    if (key === "message") label.textContent = "メッセージ（Discord / Slack: 2000文字、Mastodon: 500文字、X: 140文字）";
    if (target.kind === "x" && ["id", "kind", "url"].includes(key)) { if (key === "kind") input.disabled = true; else input.readOnly = true; }
  }
  const update = () => { fields.token.disabled = fields.kind.value !== "mastodon"; fields.token.required = !fields.token.disabled; if (target.kind === "x") fields.kind.disabled = true; };
  fields.kind.addEventListener("change", update); update();
  const remove = document.createElement("button"); remove.type = "button"; remove.className = "danger"; remove.textContent = "削除"; remove.onclick = () => row.remove(); row.append(remove);
  row.read = () => ({ target: { id: fields.id.value.trim(), kind: fields.kind.value, url: fields.url.value.trim(), token: fields.token.disabled ? "" : fields.token.value.trim() }, message: fields.message.value });
  el("targets").append(row);
}
async function refresh() {
  const status = await api("/status");
  locked = status.suspectedDeath;
  const reasons = { monitoring: "監視中", setup_required: "初期設定が必要です", waiting_for_first_heart_rate: "初回心拍を待っています", battery_empty: "電池切れで停止中", battery_unknown: "電池状態が不明", sync_stale: "同期が古いため停止中", health_api_unavailable: "Health APIに接続できません" };
  const time = value => !value || value.startsWith("0001-") ? "未取得" : new Date(value).toLocaleString("ja-JP");
  el("status").textContent = "状態: " + (reasons[status.reason] || status.reason || "未チェック") + "\n最終心拍: " + time(status.lastHeartRate) + "\n最終確認: " + time(status.lastCheck) + "\n累積欠落時間: " + (status.missingSeconds / 3600).toFixed(2) + " 時間\n判定成立: " + (status.suspectedDeath ? "はい（設定ロック中）" : "いいえ") + "\n配信済み: " + (Object.keys(status.delivered || {}).filter(id => status.delivered[id]).join(", ") || "なし") + "\nX投稿試行: " + (status.xAttempts || 0) + " / 3（上限到達後は自動再試行しません）";
  for (const control of el("settings-form").querySelectorAll("input,select,textarea,button")) control.disabled = status.suspectedDeath;
  if (!status.suspectedDeath) for (const select of el("targets").querySelectorAll("select")) select.dispatchEvent(new Event("change"));
  el("discord-connect").disabled = locked; el("x-connect").disabled = locked;
}
async function loadSettings() {
  const settings = await api("/settings");
  el("device").value = settings.deviceName || ""; el("targets").replaceChildren();
  for (const target of settings.targets || []) addTarget(target, (settings.messages || {})[target.id] || "");
  if (!el("targets").children.length) addTarget();
  await refresh(); el("admin-token").value = ""; el("login").hidden = true; el("console").hidden = false;
}
el("login-form").onsubmit = async event => {
  event.preventDefault(); token = el("admin-token").value;
  try {
    await loadSettings(); notice("接続しました");
  } catch (error) { token = ""; notice(error.message); }
};
el("add").onclick = () => addTarget();
el("refresh").onclick = () => refresh().catch(error => notice(error.message));
el("logout").onclick = async () => { if (clerkClient) { try { await clerkClient.signOut(); location.reload(); } catch { notice("ログアウトに失敗しました。再度お試しください"); } return; } token = ""; el("targets").replaceChildren(); el("device").value = ""; el("device-list").replaceChildren(); el("status").textContent = ""; el("console").hidden = true; el("login").hidden = false; notice("ログアウトしました"); };
el("settings-form").onsubmit = async event => {
  event.preventDefault();
  const settings = { deviceName: el("device").value.trim(), targets: [], messages: Object.create(null) };
  for (const row of el("targets").children) { const { target, message } = row.read(); settings.targets.push(target); if (message) settings.messages[target.id] = message; }
  el("save").disabled = true;
  try { await api("/settings", { method: "PUT", body: JSON.stringify(settings) }); notice("設定を保存しました"); await refresh(); }
  catch (error) { notice(error.message); }
  finally { el("save").disabled = locked; }
};

el("load-devices").onclick = async () => {
  try {
    const devices = await api("/devices"); el("device-list").replaceChildren();
    const placeholder = document.createElement("option"); placeholder.value = ""; placeholder.textContent = "対象の端末を選択してください"; el("device-list").append(placeholder);
    for (const device of devices) { const option = document.createElement("option"); option.value = device.name; option.textContent = device.name + " / 電池: " + (device.batteryStatus || "不明"); el("device-list").append(option); }
    notice(devices.length ? "端末を選び、設定を保存してください" : "Fitbit端末が見つかりませんでした");
  } catch (error) { notice(error.message); }
};
el("device-list").onchange = () => { if (el("device-list").value) el("device").value = el("device-list").value; };
el("account").onclick = () => clerkClient?.openUserProfile();
el("discord-connect").onclick = async () => {
  try { const result = await api("/integrations/discord", { method: "POST" }); location.assign(result.url); } catch (error) { notice(error.message); }
};
el("x-connect").onclick = async () => {
  try { await api("/integrations/x", { method: "POST" }); await loadSettings(); notice("Xを追加しました。メッセージを入力して保存してください"); } catch (error) { notice(error.message); }
};
function loadScript(src, key) {
  return new Promise((resolve, reject) => {
    const script = document.createElement("script"); script.src = src; script.crossOrigin = "anonymous";
    if (key) script.dataset.clerkPublishableKey = key;
    script.onload = resolve; script.onerror = () => reject(new Error("Clerkを読み込めませんでした")); document.head.append(script);
  });
}
async function initialize() {
  const response = await fetch("/auth/config", { cache: "no-store" });
  if (!response.ok) throw new Error("ログイン設定を取得できませんでした");
  const config = await response.json();
  if (!config.publishableKey) return;
  el("login-form").hidden = true;
  await loadScript(config.frontendOrigin + "/npm/@clerk/ui@1/dist/ui.browser.js");
  await loadScript(config.frontendOrigin + "/npm/@clerk/clerk-js@6/dist/clerk.browser.js", config.publishableKey);
  clerkClient = window.Clerk;
  await clerkClient.load({ ui: { ClerkUI: window.__internal_ClerkUICtor } });
  if (clerkClient.isSignedIn) {
    await loadSettings(); el("integrations").hidden = false;
    el("discord-connect").hidden = !config.discordEnabled; el("x-connect").hidden = !config.xEnabled;
    const result = new URLSearchParams(location.search).get("integration");
    if (result) { history.replaceState(null, "", "/"); notice(result === "connected" ? "Discordを追加しました。メッセージを入力して保存してください" : "Discord連携をキャンセルしました"); }
  } else {
    el("clerk-sign-in").hidden = false;
    clerkClient.mountSignIn(el("clerk-sign-in"), { routing: "hash", forceRedirectUrl: location.origin + "/", signUpForceRedirectUrl: location.origin + "/" });
  }
}
initialize().catch(error => notice(error.message));
