import { useState } from 'react'

interface TimeResponse {
  time: string
  filter: string
}

export default function Dashboard() {
  const [data, setData] = useState<TimeResponse | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function fetchTime() {
    setLoading(true)
    setError(null)
    try {
      const res = await fetch('/api/time')
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      setData(await res.json())
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div>
      <h2>Dashboard</h2>
      <p>
        Calls <code>GET /api/time</code> — handled directly inside the{' '}
        <code>.so</code> by the <code>api-backend</code> jisr filter.
        No upstream cluster involved.
      </p>
      <button onClick={fetchTime} disabled={loading}>
        {loading ? 'Loading…' : 'Fetch server time'}
      </button>
      {error && <p className="error">Error: {error}</p>}
      {data && (
        <pre>{JSON.stringify(data, null, 2)}</pre>
      )}
    </div>
  )
}
