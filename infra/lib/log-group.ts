import { RemovalPolicy } from 'aws-cdk-lib';
import { Role, ServicePrincipal } from 'aws-cdk-lib/aws-iam';
import { ILogGroup, LogGroup, RetentionDays } from 'aws-cdk-lib/aws-logs';
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

/**
 * Execution role for a Lambda that may write only to its own log group. Without it, CDK gives each
 * function a role with the AWSLambdaBasicExecutionRole managed policy, which allows creating log
 * groups and writing to any log group in the account. Here the log group already exists (it is
 * passed to the function as `logGroup`), so creating one is not needed, and `grantWrite` allows
 * only `logs:CreateLogStream` and `logs:PutLogEvents` on that one group. Other grants (tables,
 * queues) are added to this role by the usual `grant*` calls.
 */
export function lambdaRole(scope: Construct, id: string, logGroup: ILogGroup): Role {
  const role = new Role(scope, id, {
    assumedBy: new ServicePrincipal('lambda.amazonaws.com'),
  });
  logGroup.grantWrite(role);
  return role;
}
