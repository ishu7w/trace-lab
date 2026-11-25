# Workbench design

The page opens directly on the experiment. A compact masthead and a short explanation give the controls context. Use warm off-white, charcoal and one burnt-orange accent for the slowdown. Use system sans type for headings and monospace numbers for measurements.

The comparison shows p95 across baseline, slowdown and recovery with a shared scale. The trace inspector shows the phase, sampled request and a waterfall with one row per service. The waterfall keeps a shared time scale across every request in a run so a slow span stays visibly longer when switching phases. A selected row reveals its exact timing and span ID. Show textual values for every bar. The initial state explains how to generate measurements, and a failed run shows a readable error beside the run control.

Stack sections on narrow screens. Use native buttons, labels, focus indicators and status announcements. No animation is needed to explain the measurements.
