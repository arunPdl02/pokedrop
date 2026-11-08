import { CfnOutput, Duration, RemovalPolicy, Stack, StackProps, Tags } from 'aws-cdk-lib';
import { AttributeType, BillingMode, Table } from 'aws-cdk-lib/aws-dynamodb';
import { Queue, QueueEncryption } from 'aws-cdk-lib/aws-sqs';
import { Construct } from 'constructs';
import { Api } from './api';
import { Auth } from './auth';
import { Monitoring } from './monitoring';
import { MAX_RECEIVE_COUNT, WORKER_TIMEOUT, Worker } from './worker';

export interface PokedropStackProps extends StackProps {
  /** Deployment stage, e.g. `dev` or `test`. Everything except `prod` is destroyable. */
  readonly stage: string;
  /** Receives the alarm emails (kept out of the repo; bin/app.ts reads ALARM_EMAIL). */
  readonly alarmEmail: string;
}

/** Key and attribute names mirror backend/internal/storage/schema.go. */
export class PokedropStack extends Stack {
  public readonly inventoryTable: Table;
  public readonly reservationsTable: Table;
  public readonly reservationQueue: Queue;
  public readonly reservationDlq: Queue;
  public readonly worker: Worker;
  public readonly auth: Auth;
  public readonly api: Api;
  public readonly monitoring: Monitoring;

  constructor(scope: Construct, id: string, props: PokedropStackProps) {
    super(scope, id, props);

    Tags.of(this).add('project', 'pokedrop');

    const removalPolicy = props.stage === 'prod' ? RemovalPolicy.RETAIN : RemovalPolicy.DESTROY;

    this.inventoryTable = new Table(this, 'InventoryTable', {
      partitionKey: { name: 'cardId', type: AttributeType.STRING },
      billingMode: BillingMode.PAY_PER_REQUEST,
      removalPolicy,
    });

    this.reservationsTable = new Table(this, 'ReservationsTable', {
      partitionKey: { name: 'trainerId', type: AttributeType.STRING },
      sortKey: { name: 'requestId', type: AttributeType.STRING },
      billingMode: BillingMode.PAY_PER_REQUEST,
      removalPolicy,
    });

    this.auth = new Auth(this, 'Auth', { stage: props.stage, removalPolicy });

    // Messages that fail MAX_RECEIVE_COUNT times land here, kept for the longest SQS allows so
    // they can be inspected and replayed (docs/runbook.md).
    this.reservationDlq = new Queue(this, 'ReservationDlq', {
      encryption: QueueEncryption.SQS_MANAGED,
      enforceSSL: true,
      retentionPeriod: Duration.days(14),
      removalPolicy,
    });

    // Standard queue: the worker is idempotent, so duplicate delivery is safe.
    this.reservationQueue = new Queue(this, 'ReservationQueue', {
      encryption: QueueEncryption.SQS_MANAGED,
      enforceSSL: true,
      visibilityTimeout: Duration.seconds(WORKER_TIMEOUT.toSeconds() * 6),
      deadLetterQueue: { queue: this.reservationDlq, maxReceiveCount: MAX_RECEIVE_COUNT },
      removalPolicy,
    });

    this.api = new Api(this, 'Api', {
      auth: this.auth,
      inventoryTable: this.inventoryTable,
      reservationsTable: this.reservationsTable,
      reservationQueue: this.reservationQueue,
      removalPolicy,
    });

    this.worker = new Worker(this, 'Worker', {
      inventoryTable: this.inventoryTable,
      reservationsTable: this.reservationsTable,
      reservationQueue: this.reservationQueue,
      removalPolicy,
    });

    this.monitoring = new Monitoring(this, 'Monitoring', {
      stage: props.stage,
      alarmEmail: props.alarmEmail,
      reservationQueue: this.reservationQueue,
      reservationDlq: this.reservationDlq,
      workerFunction: this.worker.function,
      workerLogGroup: this.worker.logGroup,
    });

    new CfnOutput(this, 'InventoryTableName', { value: this.inventoryTable.tableName });
    new CfnOutput(this, 'ReservationsTableName', { value: this.reservationsTable.tableName });
    new CfnOutput(this, 'ReservationQueueUrl', { value: this.reservationQueue.queueUrl });
    new CfnOutput(this, 'ReservationDlqUrl', { value: this.reservationDlq.queueUrl });
    new CfnOutput(this, 'ReservationQueueArn', { value: this.reservationQueue.queueArn });
    new CfnOutput(this, 'ReservationDlqArn', { value: this.reservationDlq.queueArn });
    new CfnOutput(this, 'WorkerFunctionName', { value: this.worker.function.functionName });
    new CfnOutput(this, 'WorkerLogGroupName', { value: this.worker.logGroup.logGroupName });
    new CfnOutput(this, 'AlarmTopicArn', { value: this.monitoring.topic.topicArn });
    new CfnOutput(this, 'ApiUrl', { value: this.api.httpApi.apiEndpoint });
    new CfnOutput(this, 'UserPoolId', { value: this.auth.userPool.userPoolId });
    new CfnOutput(this, 'UserPoolClientId', { value: this.auth.userPoolClient.userPoolClientId });
    if (this.auth.devClient) {
      new CfnOutput(this, 'DevClientId', { value: this.auth.devClient.userPoolClientId });
    }
    new CfnOutput(this, 'HostedUiDomain', {
      value: this.auth.domain.baseUrl().replace(/^https:\/\//, ''),
    });
  }
}
