export default function About() {
  return (
    <div>
      <h2>About</h2>
      <p>
        <strong>jisr</strong> (جسر) is a net/http-style handler abstraction
        for writing Envoy dynamic module filters in Go.
      </p>
      <p>
        This page has its own URL (<code>/about</code>). Refreshing it still
        works because the filter returns <code>index.html</code> for any path
        that doesn't match a static asset, and React Router renders this
        component client-side.
      </p>
    </div>
  )
}
