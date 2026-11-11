import { execFileSync } from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import { App } from 'aws-cdk-lib';
import { Match, Template } from 'aws-cdk-lib/assertions';
import { CiStack, DEPLOY_ROLE_NAME } from '../lib/ci';

const ACCOUNT = '111111111111';
const REGION = 'us-west-2';
/**
 * What GitHub puts in the `sub` of a token from a push or dispatch on main of this repository. It
 * uses the owner and repository IDs, not their names (`gh api
 * repos/arunPdl02/pokedrop/actions/oidc/customization/sub`). Written out in full on purpose, so a
 * change to the constant in lib/ci.ts cannot silently change the test too.
 */
const MAIN_SUBJECT = 'repo:arunPdl02@135916160/pokedrop@1442086654:ref:refs/heads/main';

function synth(props: Partial<ConstructorParameters<typeof CiStack>[2]> = {}): Template {
  const app = new App();
  return Template.fromStack(
    new CiStack(app, 'pokedrop-ci', { env: { account: ACCOUNT, region: REGION }, ...props }),
  );
}

function deployRole(template: Template): Record<string, any> {
  const roles = Object.values(template.findResources('AWS::IAM::Role'));
  expect(roles).toHaveLength(1);
  return roles[0]!;
}

describe('CiStack', () => {
  const template = synth();

  test('creates the GitHub OIDC provider for the Actions audience, and nothing else of its kind', () => {
    template.resourceCountIs('AWS::IAM::OIDCProvider', 1);
    template.hasResourceProperties('AWS::IAM::OIDCProvider', {
      Url: 'https://token.actions.githubusercontent.com',
      ClientIdList: ['sts.amazonaws.com'],
    });
  });

  test('has one role and no Lambda or custom resource', () => {
    template.resourceCountIs('AWS::IAM::Role', 1);
    template.resourceCountIs('AWS::Lambda::Function', 0);
    const types = Object.values<any>(template.toJSON().Resources).map((r) => r.Type);
    expect(types.filter((t) => t.startsWith('Custom::'))).toEqual([]);
  });

  test('only the main branch of this repository can assume the role, by exact match', () => {
    const trust = deployRole(template).Properties.AssumeRolePolicyDocument;
    expect(trust.Statement).toHaveLength(1);
    const [statement] = trust.Statement;
    expect(statement).toEqual({
      Effect: 'Allow',
      Action: 'sts:AssumeRoleWithWebIdentity',
      Principal: { Federated: expect.anything() },
      Condition: {
        StringEquals: {
          'token.actions.githubusercontent.com:aud': 'sts.amazonaws.com',
          'token.actions.githubusercontent.com:sub': MAIN_SUBJECT,
        },
      },
    });
    // No pattern matching anywhere in the trust policy, and no other principal.
    expect(JSON.stringify(trust)).not.toMatch(/StringLike|ForAnyValue|ForAllValues|\*/);
    expect(Object.keys(statement.Principal)).toEqual(['Federated']);
    expect(JSON.stringify(statement.Principal.Federated)).toContain('GithubProvider');
  });

  test('trusts the immutable ID-based subject, not the name-based one that tokens do not carry', () => {
    const trust = JSON.stringify(deployRole(template).Properties.AssumeRolePolicyDocument);
    expect(trust).toContain('@135916160/pokedrop@1442086654');
    expect(trust).not.toContain('repo:arunPdl02/pokedrop');
  });

  test('the role may only assume the deploy, file-publishing and lookup bootstrap roles', () => {
    const role = deployRole(template);
    expect(role.Properties.RoleName).toBe(DEPLOY_ROLE_NAME);
    expect(role.Properties.ManagedPolicyArns).toBeUndefined();
    expect(role.Properties.MaxSessionDuration).toBe(3600);

    const policies = Object.values<any>(template.findResources('AWS::IAM::Policy'));
    expect(policies).toHaveLength(1);
    const statements = policies[0].Properties.PolicyDocument.Statement;
    expect(statements).toHaveLength(1);
    expect(statements[0].Effect).toBe('Allow');
    expect(statements[0].Action).toBe('sts:AssumeRole');
    const resources = (statements[0].Resource as unknown[]).map((r) => JSON.stringify(r));
    const expected = ['deploy', 'file-publishing', 'lookup'].map(
      (type) => `cdk-hnb659fds-${type}-role-${ACCOUNT}-${REGION}`,
    );
    expect(resources).toHaveLength(3);
    expected.forEach((name, i) => expect(resources[i]).toContain(`:role/${name}"`));
    expect(JSON.stringify(statements)).not.toContain('*');
    expect(JSON.stringify(statements)).not.toContain('image-publishing');
  });

  test('is tagged project=pokedrop', () => {
    const [role] = Object.values<any>(template.findResources('AWS::IAM::Role'));
    expect(role.Properties.Tags).toEqual(
      expect.arrayContaining([{ Key: 'project', Value: 'pokedrop' }]),
    );
  });

  test('can use an existing provider instead of creating a second one', () => {
    const arn = `arn:aws:iam::${ACCOUNT}:oidc-provider/token.actions.githubusercontent.com`;
    const existing = synth({ existingProviderArn: arn });
    existing.resourceCountIs('AWS::IAM::OIDCProvider', 0);
    const [statement] = deployRole(existing).Properties.AssumeRolePolicyDocument.Statement;
    expect(statement.Principal.Federated).toBe(arn);
    expect(statement.Condition.StringEquals['token.actions.githubusercontent.com:sub']).toBe(
      MAIN_SUBJECT,
    );
  });
});

describe('bin/app.ts', () => {
  /** Runs the real CDK app for a stage and lists the stacks it defines. */
  function stacksFor(stage: string): string[] {
    const outdir = fs.mkdtempSync(path.join(os.tmpdir(), 'pokedrop-app-'));
    try {
      execFileSync('npx', ['ts-node', '--prefer-ts-exts', 'bin/app.ts'], {
        cwd: path.join(__dirname, '..'),
        env: {
          ...process.env,
          ALARM_EMAIL: 'alarms@example.com',
          CDK_OUTDIR: outdir,
          CDK_CONTEXT_JSON: JSON.stringify({ stage }),
        },
        stdio: 'pipe',
      });
      const manifest = JSON.parse(fs.readFileSync(path.join(outdir, 'manifest.json'), 'utf8'));
      return Object.keys(manifest.artifacts)
        .filter((name) => manifest.artifacts[name].type === 'aws:cloudformation:stack')
        .sort();
    } finally {
      fs.rmSync(outdir, { recursive: true, force: true });
    }
  }

  test('defines the deploy role stack for prod only', () => {
    expect(stacksFor('prod')).toEqual(['pokedrop-ci', 'pokedrop-prod']);
    expect(stacksFor('dev')).toEqual(['pokedrop-dev']);
    expect(stacksFor('test')).toEqual(['pokedrop-test']);
  }, 120_000);
});
