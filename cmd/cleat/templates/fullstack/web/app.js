// This page is served by proxy/main.go (`make web`), so every call here is same-origin and holds no key: the
// proxy adds the tenant API key on the server, which is where it belongs (a key in a browser identifies the whole
// tenant). The worker sends no CORS headers, so the page could not call it directly from anywhere else. The
// proxy answers only the two calls below. To use your own backend instead, keep the two routes and have it
// authenticate the user (the oauth-provider plugin issues sessions) before it calls cleat with the key.
const API = "";
const statusEl = document.getElementById("status");

document.getElementById("go").onclick = async () => {
  statusEl.textContent = "starting...";
  const res = await fetch(`${API}/api/workflows/my-fullstack-app/start`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      // A retry of this exact request returns the original run rather than
      // creating a second one. The response says which happened.
      "Idempotency-Key": crypto.randomUUID(),
    },
    body: JSON.stringify({ input: { item: document.getElementById("item").value, qty: 1 } }),
  });
  if (!res.ok) { statusEl.textContent = `start failed: ${res.status}`; return; }
  const { id } = await res.json();
  poll(id);
};

// The browser polls published state rather than waiting on the run. See
// README.md, "Why the browser does not wait".
async function poll(id) {
  for (let i = 0; i < 60; i++) {
    const r = await fetch(`${API}/api/workflows/${id}/query?key=status`);
    if (r.ok) {
      const { value } = await r.json();
      statusEl.textContent = `${id}: ${value || "(no state yet)"}`;
      if (value === "complete" || value === "rejected") return;
    }
    await new Promise(s => setTimeout(s, 500));
  }
  statusEl.textContent = `${id}: gave up polling`;
}
