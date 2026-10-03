// Saturation: the baseline P2P workload at stepped arrival rates (PLATEAUS,
// default 2000..6000/s, STEP long each). Each plateau is its own scenario, so
// the summary reports achieved rate, dropped iterations and p99 per offered
// rate. Capacity is the highest plateau that keeps up: no material drops, and
// a prompt capture drain afterwards (run.sh). Rung 4 ratios (README) compare
// capacities, since every engine keeps up with a fixed 2,000/s.
import transfer, { setup } from './baseline.js';

const PLATEAUS = (__ENV.PLATEAUS || '2000,3000,4000,5000,6000').split(',').map(Number);
const STEP = Number(__ENV.STEP_SECONDS || 10);

const scenarios = {};
const thresholds = {};
PLATEAUS.forEach((rate, i) => {
  const name = `r${rate}`;
  scenarios[name] = {
    executor: 'constant-arrival-rate',
    rate,
    timeUnit: '1s',
    duration: `${STEP}s`,
    startTime: `${i * STEP}s`,
    preAllocatedVUs: Number(__ENV.VUS || 200),
    maxVUs: Number(__ENV.MAX_VUS || 1000),
  };
  // Thresholds that always pass make k6 export each plateau's submetrics.
  thresholds[`iterations{scenario:${name}}`] = ['count>=0'];
  thresholds[`dropped_iterations{scenario:${name}}`] = ['count>=0'];
  thresholds[`grpc_req_duration{scenario:${name}}`] = ['p(99)>=0'];
});

export const options = {
  scenarios,
  thresholds,
  setupTimeout: '120s',
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

export { setup };
export default transfer;
