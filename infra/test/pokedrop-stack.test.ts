import { App, Stack } from 'aws-cdk-lib';
import { Match, Template } from 'aws-cdk-lib/assertions';
import { PokedropStack } from '../lib/pokedrop-stack';
import { serviceLogGroup } from '../lib/log-group';

function synth(stage: string): Template {
  const app = new App();
  return Template.fromStack(new PokedropStack(app, `pokedrop-${stage}`, { stage }));
}

describe('PokedropStack', () => {
  const template = synth('dev');

  test('creates exactly the inventory and reservations tables', () => {
    template.resourceCountIs('AWS::DynamoDB::Table', 2);
  });

  test('inventory is keyed by cardId', () => {
    template.hasResourceProperties('AWS::DynamoDB::Table', {
      KeySchema: [{ AttributeName: 'cardId', KeyType: 'HASH' }],
    });
  });

  test('reservations are keyed by trainerId + requestId', () => {
    template.hasResourceProperties('AWS::DynamoDB::Table', {
      KeySchema: [
        { AttributeName: 'trainerId', KeyType: 'HASH' },
        { AttributeName: 'requestId', KeyType: 'RANGE' },
      ],
    });
  });

  test('tables are on-demand and tagged project=pokedrop', () => {
    const tables = template.findResources('AWS::DynamoDB::Table');
    for (const table of Object.values(tables)) {
      expect(table.Properties.BillingMode).toBe('PAY_PER_REQUEST');
      expect(table.Properties.Tags).toEqual(
        expect.arrayContaining([{ Key: 'project', Value: 'pokedrop' }]),
      );
    }
  });

  test('dev tables are destroyed with the stack', () => {
    const tables = template.findResources('AWS::DynamoDB::Table');
    for (const table of Object.values(tables)) {
      expect(table.DeletionPolicy).toBe('Delete');
    }
  });

  test('prod tables are retained', () => {
    const tables = synth('prod').findResources('AWS::DynamoDB::Table');
    for (const table of Object.values(tables)) {
      expect(table.DeletionPolicy).toBe('Retain');
    }
  });

  test('exposes table names as outputs', () => {
    template.hasOutput('InventoryTableName', { Value: Match.anyValue() });
    template.hasOutput('ReservationsTableName', { Value: Match.anyValue() });
  });
});

describe('PokedropStack authentication', () => {
  const template = synth('dev');
  const clients = Object.values(template.findResources('AWS::Cognito::UserPoolClient')).map(
    (c) => c.Properties,
  );
  const frontend = clients.find((c) => c.AllowedOAuthFlowsUserPoolClient === true);
  const dev = clients.find((c) => c.AllowedOAuthFlowsUserPoolClient !== true);

  test('user pool signs in with email and allows self sign-up', () => {
    template.resourceCountIs('AWS::Cognito::UserPool', 1);
    template.hasResourceProperties('AWS::Cognito::UserPool', {
      UsernameAttributes: ['email'],
      AutoVerifiedAttributes: ['email'],
      AdminCreateUserConfig: { AllowAdminCreateUserOnly: false },
    });
  });

  test('hosted UI domain is a Cognito prefix domain', () => {
    template.resourceCountIs('AWS::Cognito::UserPoolDomain', 1);
    template.hasResourceProperties('AWS::Cognito::UserPoolDomain', {
      Domain: Match.anyValue(),
    });
  });

  test('frontend client uses authorization code grant only, with localhost callbacks', () => {
    expect(frontend).toBeDefined();
    expect(frontend!.AllowedOAuthFlows).toEqual(['code']);
    expect(frontend!.AllowedOAuthScopes).toEqual(
      expect.arrayContaining(['openid', 'email', 'aws.cognito.signin.user.admin']),
    );
    expect(frontend!.CallbackURLs).toEqual(['http://localhost:5173']);
    expect(frontend!.LogoutURLs).toEqual(['http://localhost:5173']);
    expect(frontend!.GenerateSecret).toBeFalsy();
    expect(frontend!.ExplicitAuthFlows ?? []).not.toContain('ALLOW_ADMIN_USER_PASSWORD_AUTH');
  });

  test('dev client allows only admin password auth, with no OAuth and no secret', () => {
    expect(dev).toBeDefined();
    expect(dev!.ExplicitAuthFlows).toContain('ALLOW_ADMIN_USER_PASSWORD_AUTH');
    expect(dev!.ExplicitAuthFlows).not.toContain('ALLOW_USER_PASSWORD_AUTH');
    expect(dev!.AllowedOAuthFlows).toBeUndefined();
    expect(dev!.GenerateSecret).toBeFalsy();
  });

  test('prod has no dev client and retains the user pool', () => {
    const prod = synth('prod');
    prod.resourceCountIs('AWS::Cognito::UserPoolClient', 1);
    const clientProps = Object.values(prod.findResources('AWS::Cognito::UserPoolClient'));
    expect(JSON.stringify(clientProps)).not.toContain('ALLOW_ADMIN_USER_PASSWORD_AUTH');
    for (const pool of Object.values(prod.findResources('AWS::Cognito::UserPool'))) {
      expect(pool.DeletionPolicy).toBe('Retain');
    }
    prod.hasOutput('UserPoolClientId', { Value: Match.anyValue() });
    expect(prod.toJSON().Outputs.DevClientId).toBeUndefined();
  });

  test('dev user pool is destroyed with the stack', () => {
    for (const pool of Object.values(template.findResources('AWS::Cognito::UserPool'))) {
      expect(pool.DeletionPolicy).toBe('Delete');
    }
  });

  test('exposes the frontend outputs', () => {
    for (const name of ['UserPoolId', 'UserPoolClientId', 'DevClientId', 'HostedUiDomain']) {
      template.hasOutput(name, { Value: Match.anyValue() });
    }
  });
});

describe('serviceLogGroup', () => {
  test('retains logs for one week and deletes with the stack', () => {
    const stack = new Stack();
    serviceLogGroup(stack, 'ApiLogs');
    const template = Template.fromStack(stack);

    template.hasResourceProperties('AWS::Logs::LogGroup', { RetentionInDays: 7 });
    const groups = template.findResources('AWS::Logs::LogGroup');
    for (const group of Object.values(groups)) {
      expect(group.DeletionPolicy).toBe('Delete');
    }
  });
});
