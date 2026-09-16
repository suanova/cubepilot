// The write-confirmation card (issue #20 HITL): a matched write is parked until
// a human approves or rejects it.
//
// Extracted from the thread because it is drawn in two places now (issue #204):
// the composer carries it while it is pending -- the composer does not scroll,
// and an approval whose buttons have to be hunted for is one the turn waits on
// for no reason -- and the thread carries it once it is decided, as a record of
// what was approved, next to the work it let through.
import { ToolIcon } from './icons'
import type { BubbleConfirm } from './model'

export function ApprovalCard({
  confirm,
  allowAlwaysOk,
  onDecide,
}: {
  confirm: BubbleConfirm
  allowAlwaysOk: boolean
  onDecide: (confirm: BubbleConfirm, decision: 'approve' | 'reject' | 'allow-always') => void
}) {
  return (
    <div className="tool-card" style={{ borderColor: 'rgba(245,158,11,.45)' }}>
      <div className="tool-head">
        <ToolIcon />
        <span className="tool-cmd">Write confirmation</span>
        {confirm.resolved && confirm.approved === undefined ? (
          // Settled without a decision: the turn was stopped while this write
          // was parked. Neutral, not a rejection the user never made.
          <span className="pill neutral">Stopped</span>
        ) : confirm.resolved ? (
          <span className={`pill ${confirm.approved ? 'success' : 'danger'}`}>
            {confirm.approved ? 'Approved' : 'Rejected'}
          </span>
        ) : (
          <span className="pill warn">Awaiting your decision</span>
        )}
      </div>
      {/* What the write is, and why the agent wants it: the part that scrolls
          when the dock cannot give the card its full height. The decision row
          and the reason a decision failed are the controls and their feedback,
          so they stay put -- a control that scrolls out of the box it was put
          in is the failure the dock exists to prevent (issue #204). */}
      <div className="approval-body">
        {confirm.command && (
          <div className="tool-body">
            <span className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{confirm.command}</span>
          </div>
        )}
        {confirm.message && (
          <div style={{ fontSize: 12.5, color: 'var(--muted, rgba(0,0,0,.55))', marginTop: 6, lineHeight: 1.5 }}>{confirm.message}</div>
        )}
      </div>
      {!confirm.resolved && (
        <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
          <button
            className="btn sm"
            onClick={() => onDecide(confirm, 'reject')}
            disabled={!!confirm.busy}
            style={{ border: '1px solid var(--danger)', color: 'var(--danger)' }}
          >
            Reject
          </button>
          <button className="btn sm primary" onClick={() => onDecide(confirm, 'approve')} disabled={!!confirm.busy}>
            {confirm.busy ? 'Sending…' : 'Approve'}
          </button>
          {allowAlwaysOk && (
            <button
              className="btn sm"
              onClick={() => onDecide(confirm, 'allow-always')}
              disabled={!!confirm.busy}
              title="Approve and add this command to your allowlist so it no longer asks"
            >
              Always allow
            </button>
          )}
        </div>
      )}
      {confirm.error && (
        <div style={{ fontSize: 12.5, color: 'var(--danger)', marginTop: 6 }}>{confirm.error}</div>
      )}
    </div>
  )
}
