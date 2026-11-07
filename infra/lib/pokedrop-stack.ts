import { CfnOutput, RemovalPolicy, Stack, StackProps, Tags } from 'aws-cdk-lib';
import { AttributeType, BillingMode, Table } from 'aws-cdk-lib/aws-dynamodb';
import { Queue, QueueEncryption } from 'aws-cdk-lib/aws-sqs';
import { Construct } from 'constructs';
import { Api } from './api';
import { Auth } from './auth';

export interface PokedropStackProps extends StackProps {
  /** Deployment stage, e.g. `dev` or `test`. Everything except `prod` is destroyable. */
  readonly stage: string;
}

/** Key and attribute names mirror backend/internal/storage/schema.go. */
export class PokedropStack extends Stack {
  public readonly inventoryTable: Table;
  public readonly reservationsTable: Table;
  public readonly reservationQueue: Queue;
  public readonly auth: Auth;
  public readonly api: Api;

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

    // Standard queue: the worker is idempotent, so duplicate delivery is safe. The dead letter
    // queue, retry limits and alarms arrive in Phase 5.
    this.reservationQueue = new Queue(this, 'ReservationQueue', {
      encryption: QueueEncryption.SQS_MANAGED,
      enforceSSL: true,
      removalPolicy,
    });

    this.api = new Api(this, 'Api', {
      auth: this.auth,
      inventoryTable: this.inventoryTable,
      reservationsTable: this.reservationsTable,
      reservationQueue: this.reservationQueue,
      removalPolicy,
    });

    new CfnOutput(this, 'InventoryTableName', { value: this.inventoryTable.tableName });
    new CfnOutput(this, 'ReservationsTableName', { value: this.reservationsTable.tableName });
    new CfnOutput(this, 'ReservationQueueUrl', { value: this.reservationQueue.queueUrl });
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
