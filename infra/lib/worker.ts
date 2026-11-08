import * as path from 'path';
import { Duration, RemovalPolicy } from 'aws-cdk-lib';
import { Table } from 'aws-cdk-lib/aws-dynamodb';
import { Architecture, Code, Function as LambdaFunction, Runtime } from 'aws-cdk-lib/aws-lambda';
import { SqsEventSource } from 'aws-cdk-lib/aws-lambda-event-sources';
import { LogGroup } from 'aws-cdk-lib/aws-logs';
import { IQueue } from 'aws-cdk-lib/aws-sqs';
import { Construct } from 'constructs';
import { serviceLogGroup } from './log-group';

/**
 * Worker Lambda timeout. The reservation queue's visibility timeout is derived from it (6×, the
 * AWS recommendation for Lambda event sources), so the two cannot drift apart.
 */
export const WORKER_TIMEOUT = Duration.seconds(10);

/** Receives before SQS moves a message to the dead letter queue. */
export const MAX_RECEIVE_COUNT = 5;

/** Records per invocation; failed ones are reported individually (ReportBatchItemFailures). */
export const WORKER_BATCH_SIZE = 10;

/**
 * Concurrent worker invocations. Every reservation for one card updates the same inventory item,
 * so more concurrency mostly adds transaction conflicts on a hot card rather than throughput.
 */
export const WORKER_MAX_CONCURRENCY = 5;

export interface WorkerProps {
  readonly inventoryTable: Table;
  readonly reservationsTable: Table;
  /** Queue the worker consumes. Its dead letter queue is handled by SQS, not the worker. */
  readonly reservationQueue: IQueue;
  readonly removalPolicy: RemovalPolicy;
}

/** Directory holding the cross-compiled worker `bootstrap` (built by `make build-lambda`). */
const WORKER_CODE_DIR = path.join(__dirname, '..', '..', 'backend', 'build', 'worker');

/** Go Lambda that turns queued reservation requests into CONFIRMED / SOLD_OUT outcomes. */
export class Worker extends Construct {
  public readonly function: LambdaFunction;
  public readonly logGroup: LogGroup;

  constructor(scope: Construct, id: string, props: WorkerProps) {
    super(scope, id);

    this.logGroup = serviceLogGroup(this, 'Logs', props.removalPolicy);

    this.function = new LambdaFunction(this, 'Function', {
      runtime: Runtime.PROVIDED_AL2023,
      architecture: Architecture.ARM_64,
      handler: 'bootstrap',
      code: Code.fromAsset(WORKER_CODE_DIR),
      memorySize: 256,
      timeout: WORKER_TIMEOUT,
      logGroup: this.logGroup,
      // Names mirror backend/internal/storage/schema.go.
      environment: {
        INVENTORY_TABLE: props.inventoryTable.tableName,
        RESERVATIONS_TABLE: props.reservationsTable.tableName,
      },
    });

    // Least privilege. TransactWriteItems has no IAM action of its own: each item is authorized
    // as its own action (Update on inventory, Put on reservations). GetItem is the fallback read
    // of an existing outcome. No Scan, Query, Delete or batch writes.
    props.inventoryTable.grant(this.function, 'dynamodb:UpdateItem');
    props.reservationsTable.grant(this.function, 'dynamodb:PutItem', 'dynamodb:GetItem');

    // Grants consume-only access to the queue (receive, delete, change visibility).
    this.function.addEventSource(
      new SqsEventSource(props.reservationQueue, {
        batchSize: WORKER_BATCH_SIZE,
        reportBatchItemFailures: true,
        maxConcurrency: WORKER_MAX_CONCURRENCY,
      }),
    );
  }
}
