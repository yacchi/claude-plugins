// orchestra's Node CPU cap, preloaded with NODE_OPTIONS=--require by
// agent-exec (config.NodeCapEntries). With ORCHESTRA_NODE_CPUS a positive
// integer N, os.availableParallelism() reports N and os.cpus() its first N
// entries, so jest, vitest and every other pool sized from them stays within
// the share of the cores one test slot gets. Anything else: no change.
'use strict';
(function () {
    const raw = process.env.ORCHESTRA_NODE_CPUS;
    if (typeof raw !== 'string' || !/^[0-9]+$/.test(raw)) {
        return;
    }
    const n = Number(raw);
    if (!Number.isSafeInteger(n) || n < 1) {
        return;
    }
    const os = require('os');
    const cpus = os.cpus;
    os.availableParallelism = () => n;
    os.cpus = function () {
        const list = cpus.apply(this, arguments);
        if (list.length === 0) {
            return list;
        }
        const out = list.slice(0, n);
        while (out.length < n) {
            out.push(out[out.length - 1]);
        }
        return out;
    };
    require('module').syncBuiltinESMExports();
})();
