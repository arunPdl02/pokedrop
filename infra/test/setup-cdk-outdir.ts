import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';

// Every `new App()` stages its own copy of the 41 MB Lambda bundle in a fresh /tmp/cdk.out<random>
// that nothing deletes, and the stack tests build about sixty Apps per run. Left alone that filled
// a 7.8 GB /tmp with 440 such directories. One output directory per test file lets CDK stage each
// asset once (it skips an asset that is already there), and it is removed when the file is done.
const outdir = fs.mkdtempSync(path.join(os.tmpdir(), 'pokedrop-cdk-test-'));
process.env.CDK_OUTDIR = outdir;

afterAll(() => {
  fs.rmSync(outdir, { recursive: true, force: true });
});
