import { App } from 'aws-cdk-lib';
import { PokedropStack } from '../lib/pokedrop-stack';

const app = new App();

const stage = String(app.node.tryGetContext('stage') ?? 'dev');
if (!/^[a-z][a-z0-9-]{0,15}$/.test(stage)) {
  throw new Error(`Invalid stage "${stage}": use lowercase letters, digits and dashes`);
}

new PokedropStack(app, `pokedrop-${stage}`, {
  stage,
  env: { account: process.env.CDK_DEFAULT_ACCOUNT, region: 'us-west-2' },
});
