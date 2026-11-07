import { Duration } from 'aws-cdk-lib';

/**
 * Worker Lambda timeout. The reservation queue's visibility timeout is derived from it (6×, the
 * AWS recommendation for Lambda event sources), so the two cannot drift apart.
 */
export const WORKER_TIMEOUT = Duration.seconds(10);

/** Receives before SQS moves a message to the dead letter queue. */
export const MAX_RECEIVE_COUNT = 5;
