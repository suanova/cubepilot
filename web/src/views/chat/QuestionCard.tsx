// The ask-user card (issue #161): the agent is parked on a human answer.
import type { QuestionItem } from '@/api/types'
import { ToolIcon } from './icons'
import { questionAnswered, type BubbleQuestion } from './model'

export function QuestionCard({
  question,
  onPick,
  onSubmit,
  onDismiss,
}: {
  question: BubbleQuestion
  onPick: (q: BubbleQuestion, item: QuestionItem, label: string) => void
  onSubmit: (q: BubbleQuestion) => void
  onDismiss: (q: BubbleQuestion) => void
}) {
  const remaining = question.deadline ? Math.max(0, Math.round((question.deadline - Date.now()) / 1000)) : undefined
  const settled = question.resolved
  // The local countdown has run out but the gateway has not settled the
  // question yet: it is about to (or already has). Stop offering the controls
  // rather than let the user click into a 409, but do not claim "Expired"
  // ourselves -- that is the gateway's call, and only it can say so.
  const expiring = !settled && remaining === 0
  const locked = settled || expiring
  const outcomeLabel: Record<string, string> = {
    answered: 'Answered',
    cancelled: 'Dismissed',
    expired: 'Expired',
  }
  return (
    <div className="tool-card" style={{ borderColor: 'rgba(59,130,246,.45)' }}>
      <div className="tool-head">
        <ToolIcon />
        <span className="tool-cmd">Question from the agent</span>
        {settled ? (
          <span
            style={{
              fontSize: 12,
              borderRadius: 999,
              padding: '2px 10px',
              background: question.outcome === 'answered' ? 'rgba(34,197,94,.15)' : 'rgba(0,0,0,.07)',
              color: question.outcome === 'answered' ? '#15803d' : 'rgba(0,0,0,.55)',
            }}
          >
            {outcomeLabel[question.outcome || ''] || 'Closed'}
          </span>
        ) : (
          <span
            style={{
              fontSize: 12,
              borderRadius: 999,
              padding: '2px 10px',
              background: 'rgba(59,130,246,.15)',
              color: '#1d4ed8',
            }}
          >
            {expiring ? 'Expiring…' : `Awaiting your answer${remaining !== undefined ? ` · ${remaining}s` : ''}`}
          </span>
        )}
      </div>
      {question.items.map((item) => {
        const picked = question.picked[item.questionId] || []
        return (
          <div key={item.questionId} style={{ padding: '10px 12px', borderTop: '1px solid var(--border)' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 4 }}>
              {item.header && (
                <span style={{ fontSize: 11, letterSpacing: '.04em', textTransform: 'uppercase', color: 'var(--muted, rgba(0,0,0,.55))' }}>
                  {item.header}
                </span>
              )}
              {item.multiSelect && (
                <span style={{ fontSize: 11, color: 'var(--muted, rgba(0,0,0,.55))' }}>select one or more</span>
              )}
            </div>
            <div style={{ fontSize: 13.5, lineHeight: 1.6, marginBottom: 8 }}>{item.question}</div>
            {/* role=group + aria-label give the options an accessible group and
                name; the selected state itself is exposed by aria-pressed on
                each button rather than by colour alone. */}
            <div role="group" aria-label={item.question} style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
              {item.options.map((o) => {
                const active = picked.includes(o.label)
                return (
                  <button
                    key={o.label}
                    onClick={() => onPick(question, item, o.label)}
                    disabled={locked || !!question.busy}
                    aria-pressed={active}
                    title={o.description}
                    style={{
                      background: active ? 'var(--accent, #3b82f6)' : 'none',
                      border: `1px solid ${active ? 'var(--accent, #3b82f6)' : 'var(--border)'}`,
                      color: active ? '#fff' : 'inherit',
                      borderRadius: 6,
                      padding: '6px 14px',
                      cursor: settled ? 'default' : 'pointer',
                      fontSize: 13,
                      textAlign: 'left',
                    }}
                  >
                    {o.label}
                    {o.description && (
                      <span style={{ display: 'block', fontSize: 11.5, opacity: 0.8, marginTop: 2 }}>{o.description}</span>
                    )}
                  </button>
                )
              })}
            </div>
          </div>
        )
      })}
      {!settled && (
        <div style={{ display: 'flex', gap: 8, padding: '0 12px 12px' }}>
          <button
            onClick={() => onDismiss(question)}
            disabled={locked || !!question.busy}
            title="Dismiss the question and let the agent continue without an answer"
            style={{
              background: 'none',
              border: '1px solid var(--border)',
              borderRadius: 6,
              padding: '6px 14px',
              cursor: locked ? 'default' : 'pointer',
              opacity: locked ? 0.5 : 1,
              fontSize: 13,
            }}
          >
            Dismiss
          </button>
          <button
            onClick={() => onSubmit(question)}
            disabled={locked || !!question.busy || !questionAnswered(question)}
            style={{
              background: 'var(--accent, #3b82f6)',
              border: 'none',
              color: '#fff',
              borderRadius: 6,
              padding: '6px 14px',
              cursor: !locked && questionAnswered(question) ? 'pointer' : 'default',
              opacity: locked || !questionAnswered(question) ? 0.5 : 1,
              fontSize: 13,
            }}
          >
            {question.busy ? 'Sending…' : 'Submit'}
          </button>
        </div>
      )}
      {question.error && (
        <div style={{ fontSize: 12.5, color: 'var(--danger)', padding: '0 12px 12px' }}>{question.error}</div>
      )}
    </div>
  )
}

