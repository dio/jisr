// SPA entry — calls /api/hello through the Envoy proxy.
// In a real Vite app this would be the built output of main.tsx.

const app = document.getElementById('app');

app.innerHTML = `
  <h1>jisr SPA example</h1>
  <p style="margin-bottom:1.5rem;color:#aaa">
    Assets served directly from the .so via <code>w.SendBytes</code>.
    API calls forwarded upstream by Envoy.
  </p>
  <button id="btn">Call /api/hello</button>
  <div id="result">Press the button to call the API.</div>
`;

document.getElementById('btn').addEventListener('click', async () => {
  const btn = document.getElementById('btn');
  const result = document.getElementById('result');

  btn.disabled = true;
  result.textContent = 'Loading…';
  result.className = '';

  try {
    const res = await fetch('/api/hello');
    const data = await res.json();
    result.textContent = JSON.stringify(data, null, 2);
  } catch (err) {
    result.textContent = `Error: ${err.message}`;
    result.className = 'error';
  } finally {
    btn.disabled = false;
  }
});
