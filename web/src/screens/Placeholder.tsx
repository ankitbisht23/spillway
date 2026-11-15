/** A screen whose task has not landed yet (ADR-0056); it calls no API. */
export function Placeholder({ title, task }: { title: string; task: string }) {
  return (
    <section>
      <h1>{title}</h1>
      <p className="muted">Arrives with m6-{task}.</p>
    </section>
  );
}
