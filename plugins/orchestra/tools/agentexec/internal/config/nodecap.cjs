// orchestra's Node preload, loaded with NODE_OPTIONS=--require by agent-exec
// (config.NodeCapEntries). First it stops a forked child (one with an IPC
// channel) whose execArgv holds an eval flag: such a child runs the eval
// script again, so a `node -e` that starts jest gets workers that each start
// jest again, a fork bomb this stops in its first generation. Then the CPU
// cap: with ORCHESTRA_NODE_CPUS a positive integer N, os.availableParallelism()
// reports N and os.cpus() its first N entries, so jest, vitest and every other
// pool sized from them stays within the share of the cores one test slot
// gets. Anything else: no change to os.
'use strict';
(function () {
    if (typeof process.send === 'function') {
        const evalFlags = ['-e', '--eval', '-p', '--print', '-pe', '-ep'];
        const hit = process.execArgv.find((arg) => evalFlags.includes(arg) ||
            arg.startsWith('--eval=') || arg.startsWith('--print='));
        if (hit !== undefined) {
            require('fs').writeSync(2, "orchestra: refusing to start a forked child of 'node " + hit.split('=')[0] +
                "': it would run the eval script again and multiply (a fork bomb). Run the test runner through the package's test script, not node -e.\n");
            process.exit(1);
        }
    }
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
