import { RemovalPolicy } from 'aws-cdk-lib';
import { LogGroup, RetentionDays } from 'aws-cdk-lib/aws-logs';
import { Construct } from 'constructs';

/**
 * Log group with the project-wide retention (1 week). Lambdas pass it via `logGroup` instead of
 * relying on the deprecated `logRetention`, so every log group is explicit and expires.
 */
export function serviceLogGroup(
  scope: Construct,
  id: string,
  removalPolicy: RemovalPolicy = RemovalPolicy.DESTROY,
): LogGroup {
  return new LogGroup(scope, id, {
    retention: RetentionDays.ONE_WEEK,
    removalPolicy,
  });
}
