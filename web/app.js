const run = document.querySelector('#run');
const status = document.querySelector('#status');
const comparison = document.querySelector('#comparison');
const inspector = document.querySelector('#inspector');

const names = { baseline: 'Baseline', slowdown: 'Slowdown', recovery: 'Recovery' };
const time = value => `${value.toFixed(1)} ms`;

function element(tag, text, className) {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}

function renderComparison(phases) {
  const maximum = Math.max(...phases.map(phase => phase.p95));
  comparison.replaceChildren(...phases.map(phase => {
    const row = element('div', undefined, 'metric');
    row.dataset.phase = phase.id;
    const meter = element('progress');
    meter.max = maximum;
    meter.value = phase.p95;
    meter.setAttribute('aria-label', `${names[phase.id]} p95`);
    row.append(element('span', names[phase.id]), element('strong', time(phase.p95)),
      meter, element('small', `Median ${time(phase.p50)} · ${phase.samples.length} samples`));
    return row;
  }));
}

function selectControl(title, options) {
  const label = element('label', title);
  const select = element('select');
  for (const [value, text] of options) {
    const option = element('option', text);
    option.value = value;
    select.append(option);
  }
  label.append(select);
  return [label, select];
}

function renderInspector(phases) {
  const scale = Math.max(...phases.flatMap(phase =>
    phase.samples.flatMap(sample => sample.spans.map(span => span.offset + span.duration))));

  const controls = element('div', undefined, 'controls');
  const [phaseLabel, phaseSelect] = selectControl('Phase', phases.map(({id}) => [id, names[id]]));
  const [sampleLabel, sampleSelect] = selectControl('Request', []);
  controls.append(phaseLabel, sampleLabel);
  const chart = element('div');
  const details = element('div', undefined, 'details');
  details.setAttribute('aria-live', 'polite');
  inspector.replaceChildren(controls, chart, details);
  phaseSelect.value = 'slowdown';

  function draw() {
    const phase = phases.find(item => item.id === phaseSelect.value);
    const sample = phase.samples[Number(sampleSelect.value)];
    const axis = element('div', undefined, 'axis mono');
    axis.setAttribute('aria-hidden', 'true');
    axis.append(element('span', '0'), element('span', time(scale)));
    const buttons = [];
    chart.replaceChildren(axis);

    function select(span, row) {
      for (const button of buttons) button.setAttribute('aria-pressed', String(button === row));
      details.replaceChildren(
        element('div', `${span.service} · starts at ${time(span.offset)} · lasts ${time(span.duration)}`),
        element('div', `Trace ${sample.traceId}`),
        element('div', `Span ${span.id} · parent ${span.parent}`));
    }

    for (const span of sample.spans) {
      const row = element('button', undefined, 'span');
      row.type = 'button';
      const track = element('span', undefined, 'track');
      track.setAttribute('aria-hidden', 'true');
      const bar = element('span', undefined, 'bar');
      bar.style.left = `${100 * span.offset / scale}%`;
      bar.style.width = `${100 * span.duration / scale}%`;
      track.append(bar);
      row.append(element('span', span.service), track, element('span', time(span.duration), 'mono'));
      row.addEventListener('click', () => select(span, row));
      buttons.push(row);
      chart.append(row);
    }
    const index = sample.spans.findIndex(span => span.service === 'Availability');
    select(sample.spans[index], buttons[index]);
  }

  function changePhase() {
    const phase = phases.find(item => item.id === phaseSelect.value);
    const options = phase.samples.map((item, index) => {
      const option = element('option', `#${index + 1} · ${time(item.elapsed)}`);
      option.value = index;
      return option;
    });
    sampleSelect.replaceChildren(...options);
    sampleSelect.value = phase.samples.findIndex(item => item.elapsed === phase.p95);
    draw();
  }
  phaseSelect.addEventListener('change', changePhase);
  sampleSelect.addEventListener('change', draw);
  changePhase();
}

let hasResults = false;

run.addEventListener('click', async () => {
  run.disabled = true;
  status.dataset.error = 'false';
  status.textContent = hasResults ? 'Running again · showing previous results' : 'Running 27 booking requests…';
  try {
    const response = await fetch('/api/experiment', {
      method: 'POST',
      headers: { 'X-Trace-Lab': 'run' }
    });
    if (!response.ok) throw new Error((await response.text()).trim());
    const phases = await response.json();
    renderComparison(phases);
    renderInspector(phases);
    hasResults = true;
    status.textContent = 'Complete · 27 requests across 3 phases';
  } catch (error) {
    status.dataset.error = 'true';
    status.textContent = `Experiment failed · ${error.message}${hasResults ? " · showing previous results" : ""}`;
  } finally {
    run.disabled = false;
  }
});
