# Trace Lab implementation plan

Trace Lab helps a student diagnose a slow request across services and prove that removing the cause restores performance. A reviewer can run the same experiment locally without accounts, API keys, or paid infrastructure.

## Scope and milestones

1. Document architecture, measurement, security, and screen behavior.
2. Build the Go HTTP chain and W3C trace propagation. Verify exported spans have one trace ID and correct parent links.
3. Implement a controlled baseline, slowdown, and recovery experiment. Calculate the median and p95 from observed samples. Show sample counts and individual timings.
4. Build the responsive workbench. Show the measured comparison and a selectable service waterfall. Add clear loading, error, and initial states.
5. Verify the HTTP chain, metrics, timing scenario, concurrent run guard, browser interaction, and optional Jaeger configuration. Document commands and any environment limits.

## Acceptance criteria

- One command starts a working local app on loopback.
- One run processes nine sequential requests per phase at constant workload. The lab adds 180 ms to Availability in the middle phase only. The default resume demo should show that Availability has the greatest measured increase and that p95 returns near baseline in recovery.
- Trace selection shows actual server span offsets and durations. The user can inspect any sampled request in each phase.
- Keyboard users can run an experiment, change the phase and sample, and inspect service spans. The workbench stays usable on narrow screens.
- A basic run uses no hosted services. Optional Jaeger gives access to full client and server spans.
