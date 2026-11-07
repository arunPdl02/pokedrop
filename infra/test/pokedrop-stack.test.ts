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

describe('PokedropStack API', () => {
  const template = synth('dev');

  test('API Lambda is a Go custom runtime on arm64', () => {
    template.hasResourceProperties('AWS::Lambda::Function', {
      Runtime: 'provided.al2023',
      Architectures: ['arm64'],
      Handler: 'bootstrap',
      Timeout: 10,
    });
  });

  test('Lambda logs are retained for one week and deleted with the stack', () => {
    template.hasResourceProperties('AWS::Logs::LogGroup', { RetentionInDays: 7 });
    for (const group of Object.values(template.findResources('AWS::Logs::LogGroup'))) {
      expect(group.DeletionPolicy).toBe('Delete');
    }
  });

  test('HTTP API allows CORS only from the local frontend', () => {
    template.hasResourceProperties('AWS::ApiGatewayV2::Api', {
      ProtocolType: 'HTTP',
      CorsConfiguration: {
        AllowOrigins: ['http://localhost:5173'],
        AllowMethods: ['GET', 'POST', 'OPTIONS'],
        AllowHeaders: ['Authorization', 'Content-Type'],
      },
    });
  });

  test('JWT authorizer trusts the user pool and both app clients', () => {
    template.resourceCountIs('AWS::ApiGatewayV2::Authorizer', 1);
    template.hasResourceProperties('AWS::ApiGatewayV2::Authorizer', {
      AuthorizerType: 'JWT',
      IdentitySource: ['$request.header.Authorization'],
      JwtConfiguration: {
        Issuer: Match.anyValue(),
        Audience: [
          { Ref: Match.stringLikeRegexp('FrontendClient') },
          { Ref: Match.stringLikeRegexp('DevClient') },
        ],
      },
    });
  });

  test('prod authorizer trusts only the frontend client', () => {
    synth('prod').hasResourceProperties('AWS::ApiGatewayV2::Authorizer', {
      JwtConfiguration: { Audience: [{ Ref: Match.stringLikeRegexp('FrontendClient') }] },
    });
  });

  test('every route requires the JWT authorizer and the sign-in scope', () => {
    const routes = Object.values(template.findResources('AWS::ApiGatewayV2::Route'));
    expect(routes.length).toBeGreaterThan(0);
    for (const route of routes) {
      expect(route.Properties.AuthorizationType).toBe('JWT');
      expect(route.Properties.AuthorizerId).toBeDefined();
      expect(route.Properties.AuthorizationScopes).toEqual(['aws.cognito.signin.user.admin']);
    }
  });

  test('routes are exactly the documented set', () => {
    const keys = Object.values(template.findResources('AWS::ApiGatewayV2::Route'))
      .map((r) => r.Properties.RouteKey)
      .sort();
    expect(keys).toEqual(['POST /reservations']);
  });

  test('no IAM policy grants wildcard actions', () => {
    for (const policy of Object.values(template.findResources('AWS::IAM::Policy'))) {
      for (const statement of policy.Properties.PolicyDocument.Statement) {
        const actions = ([] as string[]).concat(statement.Action);
        for (const action of actions) {
          expect(action).not.toBe('*');
          expect(action).not.toMatch(/:\*$/);
        }
      }
    }
  });

  test('exposes the API URL', () => {
    template.hasOutput('ApiUrl', { Value: Match.anyValue() });
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
