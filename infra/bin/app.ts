import { App } from 'aws-cdk-lib';
import { PokedropStack } from '../lib/pokedrop-stack';
import { parseFailAfterCommit } from '../lib/worker';

const app = new App();

const stage = String(app.node.tryGetContext('stage') ?? 'dev');
if (!/^[a-z][a-z0-9-]{0,15}$/.test(stage)) {
  throw new Error(`Invalid stage "${stage}": use lowercase letters, digits and dashes`);
}

// The alarm address stays out of the repo. Requiring it means a deploy can never silently drop
// the alarm email subscription.
const alarmEmail = process.env.ALARM_EMAIL ?? '';
if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(alarmEmail)) {
  throw new Error('Set ALARM_EMAIL to the address that receives alarm emails');
}

new PokedropStack(app, `pokedrop-${stage}`, {
  stage,
  alarmEmail,
  // `make fail-on` / `make fail-off`; the stack refuses it outside dev and test.
  failAfterCommit: parseFailAfterCommit(app.node.tryGetContext('failAfterCommit')),
  env: { account: process.env.CDK_DEFAULT_ACCOUNT, region: 'us-west-2' },
});
