package main

// dashboardHTML is a single self-contained page for parents. Token is entered
// once and kept in the browser. Designed to be usable from a phone so you can
// grant time from anywhere and the kid's lock screen clears on its next poll.
const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>rewardd</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { font: 16px/1.4 system-ui, sans-serif; margin: 0; background: #0f1115; color: #e7e9ee; }
  header { padding: 16px; border-bottom: 1px solid #232733; display:flex; gap:8px; align-items:center; }
  header h1 { font-size: 18px; margin: 0; flex: 1; }
  main { padding: 12px; max-width: 720px; margin: 0 auto; }
  input { background:#1a1d26; border:1px solid #2c313f; color:#e7e9ee; border-radius:8px; padding:8px 10px; font:inherit; }
  .card { background:#161922; border:1px solid #232733; border-radius:14px; padding:14px; margin:10px 0; }
  .row { display:flex; align-items:center; gap:10px; flex-wrap:wrap; }
  .name { font-size:18px; font-weight:600; flex:1; }
  .time { font-variant-numeric: tabular-nums; font-size:22px; font-weight:700; }
  .live { font-size:12px; color:#0f1115; background:#36d399; padding:2px 8px; border-radius:999px; font-weight:700; }
  .off  { font-size:12px; color:#8b90a0; }
  .locked { color:#ff6b6b; }
  button { font:inherit; border:0; border-radius:8px; padding:8px 12px; background:#2c313f; color:#e7e9ee; cursor:pointer; }
  button:hover { background:#3a404f; }
  button.warn { background:#5a2330; color:#ffb3b3; }
  .btns { display:flex; gap:8px; flex-wrap:wrap; margin-top:10px; }
  .muted { color:#8b90a0; font-size:13px; }
</style>
</head>
<body>
<header>
  <h1>rewardd</h1>
  <input id="tok" type="password" placeholder="API token" style="width:160px">
  <button onclick="saveTok()">Save</button>
</header>
<main>
  <div id="list"></div>
  <div class="card">
    <div class="row">
      <input id="newuser" placeholder="add kid (name)" style="flex:1">
      <button onclick="addUser()">Add</button>
    </div>
    <p class="muted">Tip: your chore system can also POST /api/v1/grant directly.</p>
  </div>
</main>
<script>
const $ = s => document.querySelector(s);
let token = localStorage.getItem("rewardd_token") || "";
$("#tok").value = token;
function saveTok(){ token = $("#tok").value.trim(); localStorage.setItem("rewardd_token", token); refresh(); }

async function api(path, method, body){
  const r = await fetch(path, {
    method, headers: { "Authorization":"Bearer "+token, "Content-Type":"application/json" },
    body: body ? JSON.stringify(body) : undefined
  });
  if(!r.ok) throw new Error(await r.text());
  return r.json();
}
function fmt(s){ s=Math.max(0,s|0); const m=(s/60)|0; const ss=s%60; return m+"m "+String(ss).padStart(2,"0")+"s"; }

async function grant(user, minutes){ try{ await api("/api/v1/grant","POST",{user, minutes, reason:"dashboard"}); refresh(); }catch(e){ alert(e.message); } }
async function lock(user){ try{ await api("/api/v1/set","POST",{user, minutes:0, reason:"dashboard lock"}); refresh(); }catch(e){ alert(e.message); } }
async function addUser(){ const n=$("#newuser").value.trim(); if(!n) return; try{ await api("/api/v1/grant","POST",{user:n, minutes:0, reason:"create"}); $("#newuser").value=""; refresh(); }catch(e){ alert(e.message); } }

async function refresh(){
  if(!token){ $("#list").innerHTML = '<div class="card muted">Enter your API token above.</div>'; return; }
  let users;
  try { users = await api("/api/v1/users","GET"); }
  catch(e){ $("#list").innerHTML = '<div class="card locked">'+e.message+'</div>'; return; }
  $("#list").innerHTML = users.map(u => {
    const live = u.playing_now ? '<span class="live">PLAYING</span>' : '<span class="off">idle</span>';
    const tcls = u.allowed ? "time" : "time locked";
    return '<div class="card">'
      + '<div class="row"><span class="name">'+esc(u.user)+'</span>'+live
      + '<span class="'+tcls+'">'+fmt(u.remaining_seconds)+'</span></div>'
      + '<div class="btns">'
      + '<button onclick="grant(\''+esc(u.user)+'\',15)">+15m</button>'
      + '<button onclick="grant(\''+esc(u.user)+'\',30)">+30m</button>'
      + '<button onclick="grant(\''+esc(u.user)+'\',60)">+60m</button>'
      + '<button class="warn" onclick="lock(\''+esc(u.user)+'\')">Lock now</button>'
      + '</div></div>';
  }).join("") || '<div class="card muted">No kids yet. Add one below.</div>';
}
function esc(s){ return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>`
