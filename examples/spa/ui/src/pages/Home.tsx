export default function Home() {
  return (
    <div>
      <h2>Home</h2>
      <p>
        This SPA is served entirely from an Envoy dynamic module <code>.so</code> —
        no file system, no separate web server.
      </p>
      <p>
        Navigate to <strong>About</strong> or <strong>Dashboard</strong> using the
        links above. Each route is handled by React Router on the client side.
        If you refresh on <code>/about</code> or <code>/dashboard</code>, the
        jisr filter returns <code>index.html</code> and React Router takes over.
      </p>
    </div>
  )
}
