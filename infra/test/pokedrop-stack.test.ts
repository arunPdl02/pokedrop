import { App, Stack } from 'aws-cdk-lib';
import { Match, Template } from 'aws-cdk-lib/assertions';
import { PokedropStack } from '../lib/pokedrop-stack';
import { serviceLogGroup } from '../lib/log-group';
import {
  FailAfterCommit,
  MAX_RECEIVE_COUNT,
  WORKER_TIMEOUT,
  parseFailAfterCommit,
} from '../lib/worker';
import { BACKLOG_AGE_THRESHOLD } from '../lib/monitoring';

function synth(stage: string, failAfterCommit?: FailAfterCommit): Template {
  const app = new App();
  return Template.fromStack(
    new PokedropStack(app, `pokedrop-${stage}`, {
      stage,
      alarmEmail: 'alarms@example.com',
      failAfterCommit,
    }),
  );
}

function workerEnv(template: Template): Record<string, unknown> {
  const fns = Object.entries(template.findResources('AWS::Lambda::Function')).filter(([id]) =>
    id.startsWith('WorkerFunction'),
  );
  expect(fns).toHaveLength(1);
  return (fns[0] as [string, any])[1].Properties.Environment.Variables;
}

interface Grant {
  actions: string[];
  /** Logical IDs of the resources the statement refers to, e.g. `InventoryTable...`. */
  resources: string[];
}

/** IAM statements attached to the role whose logical ID starts with `rolePrefix`. */
function statementsForRole(template: Template, rolePrefix: string): Grant[] {
  const grants: Grant[] = [];
  for (const policy of Object.values(template.findResources('AWS::IAM::Policy'))) {
    const roles: string[] = policy.Properties.Roles.map((r: { Ref: string }) => r.Ref);
    if (!roles.some((r) => r.startsWith(rolePrefix))) continue;
    for (const statement of policy.Properties.PolicyDocument.Statement) {
      const resources = ([] as unknown[])
        .concat(statement.Resource)
        .map((r) => JSON.stringify(r).match(/"(Fn::GetAtt|Ref)":\[?"([A-Za-z0-9]+)"/)?.[2])
        .filter((id): id is string => id !== undefined && id !== 'AWS');
      grants.push({ actions: ([] as string[]).concat(statement.Action), resources });
    }
  }
  return grants;
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
    expect(keys).toEqual(['GET /cards', 'GET /reservations/{requestId}', 'POST /reservations']);
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

  test('Lambda is configured with the table names and queue URL', () => {
    template.hasResourceProperties('AWS::Lambda::Function', {
      Environment: {
        Variables: {
          INVENTORY_TABLE: Match.anyValue(),
          RESERVATIONS_TABLE: Match.anyValue(),
          RESERVATION_QUEUE_URL: Match.anyValue(),
        },
      },
    });
  });

  test('Lambda can only read the tables and send to the queue', () => {
    const granted = statementsForRole(template, 'ApiFunctionServiceRole').flatMap((s) => s.actions);
    expect(granted.sort()).toEqual(
      [
        'dynamodb:GetItem',
        'dynamodb:Scan',
        'dynamodb:GetItem',
        'sqs:GetQueueAttributes',
        'sqs:GetQueueUrl',
        'sqs:SendMessage',
      ].sort(),
    );
    for (const action of granted) {
      expect(action).not.toMatch(/Put|Update|Delete|Transact|Receive/);
    }
  });

  test('exposes the API URL', () => {
    template.hasOutput('ApiUrl', { Value: Match.anyValue() });
  });
});

describe('PokedropStack queue', () => {
  const template = synth('dev');

  const queues = () => template.findResources('AWS::SQS::Queue');
  const logicalId = (prefix: string): string => {
    const ids = Object.keys(queues()).filter((id) => id.startsWith(prefix));
    if (ids.length !== 1) throw new Error(`expected one queue named ${prefix}*, got ${ids}`);
    return ids[0] as string;
  };
  const queueNamed = (prefix: string): any => queues()[logicalId(prefix)];

  test('has an encrypted standard reservation queue and dead letter queue', () => {
    template.resourceCountIs('AWS::SQS::Queue', 2);
    for (const queue of Object.values(queues())) {
      expect(queue.Properties.SqsManagedSseEnabled).toBe(true);
      expect(queue.Properties.FifoQueue).toBeUndefined();
    }
  });

  test('rejects non-TLS access to both queues', () => {
    const policies = Object.values(template.findResources('AWS::SQS::QueuePolicy'));
    const tlsOnly = policies.filter((p) =>
      p.Properties.PolicyDocument.Statement.some(
        (s: { Effect: string; Condition?: { Bool?: Record<string, string> } }) =>
          s.Effect === 'Deny' && s.Condition?.Bool?.['aws:SecureTransport'] === 'false',
      ),
    );
    const protectedQueues = tlsOnly.flatMap((p) =>
      p.Properties.Queues.map((q: { Ref: string }) => q.Ref),
    );
    expect(protectedQueues.sort()).toEqual(Object.keys(queues()).sort());
  });

  test('moves a message to the dead letter queue after 5 receives', () => {
    const queue = queueNamed('ReservationQueue');
    expect(queue.Properties.RedrivePolicy).toEqual({
      deadLetterTargetArn: { 'Fn::GetAtt': [logicalId('ReservationDlq'), 'Arn'] },
      maxReceiveCount: MAX_RECEIVE_COUNT,
    });
    expect(MAX_RECEIVE_COUNT).toBe(5);
  });

  test('visibility timeout is six times the worker timeout', () => {
    const queue = queueNamed('ReservationQueue');
    expect(queue.Properties.VisibilityTimeout).toBe(6 * WORKER_TIMEOUT.toSeconds());
    expect(queue.Properties.VisibilityTimeout).toBe(60);
  });

  test('dead letter queue keeps messages for 14 days', () => {
    const dlq = queueNamed('ReservationDlq');
    expect(dlq.Properties.MessageRetentionPeriod).toBe(14 * 24 * 60 * 60);
    expect(dlq.Properties.RedrivePolicy).toBeUndefined();
  });

  test('is destroyed in dev and retained in prod', () => {
    for (const q of Object.values(template.findResources('AWS::SQS::Queue'))) {
      expect(q.DeletionPolicy).toBe('Delete');
    }
    for (const q of Object.values(synth('prod').findResources('AWS::SQS::Queue'))) {
      expect(q.DeletionPolicy).toBe('Retain');
    }
  });

  test('exposes the queue URLs', () => {
    template.hasOutput('ReservationQueueUrl', { Value: Match.anyValue() });
    template.hasOutput('ReservationDlqUrl', { Value: Match.anyValue() });
  });
});

describe('PokedropStack worker', () => {
  const template = synth('dev');
  const resourcesStartingWith = (type: string, prefix: string) =>
    Object.entries(template.findResources(type)).filter(([id]) => id.startsWith(prefix));
  const workerFunction = () => {
    const fns = resourcesStartingWith('AWS::Lambda::Function', 'WorkerFunction');
    expect(fns).toHaveLength(1);
    return (fns[0] as [string, { Properties: Record<string, unknown> }])[1].Properties;
  };

  test('is a Go custom runtime on arm64 with the shared timeout', () => {
    expect(workerFunction()).toMatchObject({
      Runtime: 'provided.al2023',
      Architectures: ['arm64'],
      Handler: 'bootstrap',
      Timeout: WORKER_TIMEOUT.toSeconds(),
    });
    expect(WORKER_TIMEOUT.toSeconds()).toBe(10);
  });

  test('logs to its own one-week log group', () => {
    const logGroup = (workerFunction().LoggingConfig as { LogGroup: { Ref: string } }).LogGroup.Ref;
    expect(logGroup).toMatch(/^WorkerLogs/);
    const groups = template.findResources('AWS::Logs::LogGroup');
    expect(groups[logGroup]?.Properties.RetentionInDays).toBe(7);
    expect(groups[logGroup]?.DeletionPolicy).toBe('Delete');
  });

  test('is configured with the table names only', () => {
    const vars = (workerFunction().Environment as { Variables: Record<string, unknown> }).Variables;
    expect(Object.keys(vars).sort()).toEqual(['INVENTORY_TABLE', 'RESERVATIONS_TABLE']);
  });

  test('consumes the reservation queue in batches of 10 with partial failures and capped concurrency', () => {
    template.resourceCountIs('AWS::Lambda::EventSourceMapping', 1);
    const [, mapping] = Object.entries(
      template.findResources('AWS::Lambda::EventSourceMapping'),
    )[0] as [string, { Properties: Record<string, unknown> }];
    expect(mapping.Properties).toMatchObject({
      BatchSize: 10,
      FunctionResponseTypes: ['ReportBatchItemFailures'],
      ScalingConfig: { MaximumConcurrency: 5 },
    });
    const arn = (mapping.Properties.EventSourceArn as { 'Fn::GetAtt': string[] })['Fn::GetAtt'][0];
    expect(arn).toMatch(/^ReservationQueue/);
    expect(arn).not.toMatch(/Dlq/);
  });

  test('can update inventory, put and get reservations, and consume the queue, nothing else', () => {
    const grants = statementsForRole(template, 'WorkerFunctionServiceRole');
    const byResource = (prefix: string) =>
      grants
        .filter((g) => g.resources.some((r) => r.startsWith(prefix)))
        .flatMap((g) => g.actions)
        .sort();

    expect(byResource('InventoryTable')).toEqual(['dynamodb:UpdateItem']);
    expect(byResource('ReservationsTable')).toEqual(['dynamodb:GetItem', 'dynamodb:PutItem']);
    expect(byResource('ReservationQueue')).toEqual(
      [
        'sqs:ChangeMessageVisibility',
        'sqs:DeleteMessage',
        'sqs:GetQueueAttributes',
        'sqs:GetQueueUrl',
        'sqs:ReceiveMessage',
      ].sort(),
    );
    expect(byResource('ReservationDlq')).toEqual([]);

    const all = grants.flatMap((g) => g.actions);
    expect(all).toHaveLength(8);
    for (const action of all) {
      expect(action).not.toMatch(/SendMessage|Scan|Query|DeleteItem|Batch/);
    }
    for (const grant of grants) {
      expect(grant.resources.length).toBeGreaterThan(0);
    }
  });
});

describe('PokedropStack monitoring', () => {
  const template = synth('dev');
  const topicId = () => {
    const ids = Object.keys(template.findResources('AWS::SNS::Topic'));
    expect(ids).toHaveLength(1);
    return ids[0] as string;
  };
  const alarms = () =>
    Object.values(template.findResources('AWS::CloudWatch::Alarm')).map((a) => a.Properties);
  const alarmNamed = (name: string) => {
    const found = alarms().filter((a) => a.AlarmName === `pokedrop-dev-${name}`);
    expect(found).toHaveLength(1);
    return found[0];
  };

  test('emails the alarm address through one TLS-only topic', () => {
    template.resourceCountIs('AWS::SNS::Subscription', 1);
    template.hasResourceProperties('AWS::SNS::Subscription', {
      Protocol: 'email',
      Endpoint: 'alarms@example.com',
      TopicArn: { Ref: topicId() },
    });
    const [policy] = Object.values(template.findResources('AWS::SNS::TopicPolicy'));
    const statements = policy?.Properties.PolicyDocument.Statement;
    expect(statements).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          Effect: 'Deny',
          Condition: { Bool: { 'aws:SecureTransport': 'false' } },
        }),
        expect.objectContaining({
          Effect: 'Allow',
          Principal: { Service: 'cloudwatch.amazonaws.com' },
          Action: 'sns:Publish',
        }),
      ]),
    );
  });

  test('has exactly the four alarms, all notifying on alarm and recovery', () => {
    expect(
      alarms()
        .map((a) => a.AlarmName)
        .sort(),
    ).toEqual(
      [
        'pokedrop-dev-dlq-messages',
        'pokedrop-dev-queue-backlog-age',
        'pokedrop-dev-worker-crashes',
        'pokedrop-dev-worker-error-logs',
      ].sort(),
    );
    for (const a of alarms()) {
      expect(a.TreatMissingData).toBe('notBreaching');
      expect(a.AlarmActions).toEqual([{ Ref: topicId() }]);
      expect(a.OKActions).toEqual([{ Ref: topicId() }]);
    }
  });

  test('backlog alarm fires when the oldest message stays 300 s old for 3 minutes', () => {
    expect(alarmNamed('queue-backlog-age')).toMatchObject({
      Namespace: 'AWS/SQS',
      MetricName: 'ApproximateAgeOfOldestMessage',
      Dimensions: [
        {
          Name: 'QueueName',
          Value: { 'Fn::GetAtt': [expect.stringMatching(/^ReservationQueue/), 'QueueName'] },
        },
      ],
      Statistic: 'Maximum',
      Period: 60,
      Threshold: 300,
      ComparisonOperator: 'GreaterThanOrEqualToThreshold',
      EvaluationPeriods: 3,
      DatapointsToAlarm: 3,
    });
    expect(BACKLOG_AGE_THRESHOLD.toSeconds()).toBeGreaterThanOrEqual(
      MAX_RECEIVE_COUNT * 6 * WORKER_TIMEOUT.toSeconds(),
    );
  });

  test('dead letter alarm fires on any visible message', () => {
    expect(alarmNamed('dlq-messages')).toMatchObject({
      Namespace: 'AWS/SQS',
      MetricName: 'ApproximateNumberOfMessagesVisible',
      Dimensions: [
        {
          Name: 'QueueName',
          Value: { 'Fn::GetAtt': [expect.stringMatching(/^ReservationDlq/), 'QueueName'] },
        },
      ],
      Statistic: 'Maximum',
      Period: 60,
      Threshold: 0,
      ComparisonOperator: 'GreaterThanThreshold',
      EvaluationPeriods: 1,
    });
  });

  test('error log alarm counts ERROR lines in the worker log group', () => {
    template.resourceCountIs('AWS::Logs::MetricFilter', 1);
    const [filter] = Object.values(template.findResources('AWS::Logs::MetricFilter'));
    expect(filter?.Properties.FilterPattern).toBe('{ $.level = "ERROR" }');
    expect(filter?.Properties.LogGroupName.Ref).toMatch(/^WorkerLogs/);
    expect(filter?.Properties.MetricTransformations).toEqual([
      { MetricNamespace: 'Pokedrop/dev', MetricName: 'WorkerErrorLogs', MetricValue: '1' },
    ]);
    expect(alarmNamed('worker-error-logs')).toMatchObject({
      Namespace: 'Pokedrop/dev',
      MetricName: 'WorkerErrorLogs',
      Statistic: 'Sum',
      Period: 60,
      Threshold: 1,
      ComparisonOperator: 'GreaterThanOrEqualToThreshold',
    });
  });

  test('crash alarm adds the worker Lambda errors and throttles', () => {
    const a = alarmNamed('worker-crashes');
    expect(a).toMatchObject({ Threshold: 1, ComparisonOperator: 'GreaterThanOrEqualToThreshold' });
    const byId = Object.fromEntries(a.Metrics.map((m: { Id: string }) => [m.Id, m]));
    const expression = a.Metrics.find((m: { Expression?: string }) => m.Expression).Expression;
    expect(expression).toBe('FILL(errors, 0) + FILL(throttles, 0)');
    for (const [id, name] of [
      ['errors', 'Errors'],
      ['throttles', 'Throttles'],
    ]) {
      expect(byId[id as string]?.MetricStat).toMatchObject({
        Metric: {
          Namespace: 'AWS/Lambda',
          MetricName: name,
          Dimensions: [
            { Name: 'FunctionName', Value: { Ref: expect.stringMatching(/^WorkerFunction/) } },
          ],
        },
        Stat: 'Sum',
        Period: 60,
      });
    }
  });

  test('exposes the outputs the runbook uses', () => {
    for (const output of [
      'ReservationQueueArn',
      'ReservationDlqArn',
      'WorkerFunctionName',
      'WorkerLogGroupName',
      'AlarmTopicArn',
    ]) {
      template.hasOutput(output, { Value: Match.anyValue() });
    }
  });
});

describe('PokedropStack failure injection', () => {
  test('is off by default in every stage', () => {
    for (const stage of ['dev', 'test', 'prod']) {
      expect(workerEnv(synth(stage))).not.toHaveProperty('FAIL_AFTER_COMMIT');
    }
  });

  test('dev can enable either mode', () => {
    expect(workerEnv(synth('dev', 'once')).FAIL_AFTER_COMMIT).toBe('once');
    expect(workerEnv(synth('dev', 'always')).FAIL_AFTER_COMMIT).toBe('always');
  });

  test('prod and test cannot enable it', () => {
    for (const stage of ['prod', 'test']) {
      for (const mode of ['once', 'always'] as const) {
        expect(() => synth(stage, mode)).toThrow(/only allowed in the dev stage/);
      }
    }
  });

  test('context value must be off, once or always', () => {
    expect(parseFailAfterCommit(undefined)).toBeUndefined();
    expect(parseFailAfterCommit('off')).toBeUndefined();
    expect(parseFailAfterCommit('once')).toBe('once');
    expect(parseFailAfterCommit('always')).toBe('always');
    for (const bad of ['true', 'on', '', 'ONCE']) {
      expect(() => parseFailAfterCommit(bad)).toThrow(/Invalid failAfterCommit/);
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
