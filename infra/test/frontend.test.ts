import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import { App, Stack } from 'aws-cdk-lib';
import { Template } from 'aws-cdk-lib/assertions';
import { Frontend } from '../lib/frontend';

/** A built-site fixture: index.html and one bundle, plus a stray dev config.json if asked. */
function fixture(withConfig: boolean): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pokedrop-dist-'));
  fs.writeFileSync(path.join(dir, 'index.html'), '<!doctype html><div id="root"></div>');
  fs.mkdirSync(path.join(dir, 'assets'));
  fs.writeFileSync(path.join(dir, 'assets', 'index-abc.js'), 'console.log(1)');
  if (withConfig) {
    fs.writeFileSync(
      path.join(dir, 'config.json'),
      '{"apiUrl":"https://dev.example","clientId":"dev","hostedUiDomain":"dev.example"}',
    );
  }
  return dir;
}

/** Synthesizes a stack holding just the site; returns the deployment's first source key and the staged files. */
function synthSite(distDir: string): { builtFilesKey: string; stagedFiles: string[] } {
  const app = new App();
  const stack = new Stack(app, 'Site');
  new Frontend(stack, 'Frontend', { distDir }).deploy({
    apiUrl: 'https://api.example',
    clientId: 'client',
    hostedUiDomain: 'auth.example',
  });
  const template = Template.fromStack(stack);
  const [deployment] = Object.values<any>(template.findResources('Custom::CDKBucketDeployment'));
  const builtFilesKey: string = deployment.Properties.SourceObjectKeys[0];

  // The built files are staged as asset.<hash> next to the template; list what would be uploaded.
  const hash = builtFilesKey.replace(/\.zip$/, '');
  const staged = path.join(app.synth().directory, `asset.${hash}`);
  const stagedFiles: string[] = [];
  const walk = (dir: string, prefix = ''): void => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const rel = path.join(prefix, entry.name);
      if (entry.isDirectory()) walk(path.join(dir, entry.name), rel);
      else stagedFiles.push(rel);
    }
  };
  walk(staged);
  return { builtFilesKey, stagedFiles: stagedFiles.sort() };
}

describe('Frontend deployment', () => {
  const dirs: string[] = [];
  afterAll(() => dirs.forEach((d) => fs.rmSync(d, { recursive: true, force: true })));

  test('never uploads a config.json from the build, so only the generated one is served', () => {
    const dir = fixture(true);
    dirs.push(dir);
    const { stagedFiles } = synthSite(dir);
    expect(stagedFiles).toEqual(['assets/index-abc.js', 'index.html']);
  });

  test('uploads the same asset whether or not the build has a stray dev config.json', () => {
    const clean = fixture(false);
    const withDevConfig = fixture(true);
    dirs.push(clean, withDevConfig);
    // A developer's dist has one (make frontend-config), CI's does not; the hash must not differ,
    // or every switch between a manual and a CI deploy re-uploads the site.
    expect(synthSite(withDevConfig).builtFilesKey).toBe(synthSite(clean).builtFilesKey);
  });
});
