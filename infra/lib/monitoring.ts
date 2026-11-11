import { Duration, Stack } from 'aws-cdk-lib';
import {
  Alarm,
  ComparisonOperator,
  IMetric,
  MathExpression,
  TreatMissingData,
} from 'aws-cdk-lib/aws-cloudwatch';
import { SnsAction } from 'aws-cdk-lib/aws-cloudwatch-actions';
import { PolicyStatement, ServicePrincipal } from 'aws-cdk-lib/aws-iam';
import { IFunction } from 'aws-cdk-lib/aws-lambda';
import { FilterPattern, ILogGroup, MetricFilter } from 'aws-cdk-lib/aws-logs';
import { Subscription, SubscriptionProtocol, Topic } from 'aws-cdk-lib/aws-sns';
import { EmailSubscription } from 'aws-cdk-lib/aws-sns-subscriptions';
import { IQueue } from 'aws-cdk-lib/aws-sqs';
import { Construct } from 'constructs';

/**
 * The oldest queued message must stay at least this old for BACKLOG_EVALUATION_PERIODS minutes
 * before the backlog alarm fires. It is longer than one message's whole retry cycle
 * (MAX_RECEIVE_COUNT × visibility timeout = 5 × 60 s), so one retried or poison message does not
 * raise it.
 */
export const BACKLOG_AGE_THRESHOLD = Duration.seconds(300);
export const BACKLOG_EVALUATION_PERIODS = 3;

/** The worker's JSON logs at ERROR level: permanent failures and startup errors. */
export const WORKER_ERROR_LOG_PATTERN = FilterPattern.stringValue('$.level', '=', 'ERROR');

export interface MonitoringProps {
  readonly stage: string;
  /** Address that receives alarm and recovery emails. It must confirm the subscription. */
  readonly alarmEmail: string;
  readonly reservationQueue: IQueue;
  readonly reservationDlq: IQueue;
  readonly workerFunction: IFunction;
  readonly workerLogGroup: ILogGroup;
}

/**
 * Alarms for the reservation pipeline, sent to an SNS email topic when they fire and again when
 * they recover. Missing data is not breaching: SQS stops publishing metrics for an idle queue, and
 * the log metric has no data points until an error is logged.
 */
export class Monitoring extends Construct {
  public readonly topic: Topic;
  public readonly alarms: Alarm[] = [];

  constructor(scope: Construct, id: string, props: MonitoringProps) {
    super(scope, id);
    const stack = Stack.of(this);
    const prefix = stack.stackName;
    const period = Duration.minutes(1);

    this.topic = new Topic(this, 'AlarmTopic', {
      displayName: `PokeDrop ${props.stage} alarms`,
      enforceSSL: true,
    });
    if (props.stage === 'prod') {
      // EmailSubscription names its construct after the address, so the address ends up in the
      // logical ID, which `cdk diff` and `cdk deploy` print. CI logs of a public repo are public,
      // and GitHub masks only the exact secret, not that altered form. A fixed ID keeps the
      // address out of the output. It is prod-only because renaming dev's or test's subscription
      // would replace it and need a new email confirmation.
      new Subscription(this, 'AlarmEmail', {
        topic: this.topic,
        protocol: SubscriptionProtocol.EMAIL,
        endpoint: props.alarmEmail,
      });
    } else {
      this.topic.addSubscription(new EmailSubscription(props.alarmEmail));
    }
    // Only this stack's alarms may publish (the topic policy replaces the SNS default policy).
    this.topic.addToResourcePolicy(
      new PolicyStatement({
        principals: [new ServicePrincipal('cloudwatch.amazonaws.com')],
        actions: ['sns:Publish'],
        resources: [this.topic.topicArn],
        conditions: {
          StringEquals: { 'aws:SourceAccount': stack.account },
          ArnLike: {
            'aws:SourceArn': `arn:${stack.partition}:cloudwatch:${stack.region}:${stack.account}:alarm:${prefix}-*`,
          },
        },
      }),
    );
    const action = new SnsAction(this.topic);

    const alarm = (
      alarmId: string,
      name: string,
      description: string,
      metric: IMetric,
      threshold: number,
      comparisonOperator: ComparisonOperator,
      evaluationPeriods = 1,
    ) => {
      const a = new Alarm(this, alarmId, {
        alarmName: `${prefix}-${name}`,
        alarmDescription: `${description} See docs/runbook.md.`,
        metric,
        threshold,
        comparisonOperator,
        evaluationPeriods,
        datapointsToAlarm: evaluationPeriods,
        treatMissingData: TreatMissingData.NOT_BREACHING,
      });
      a.addAlarmAction(action);
      a.addOkAction(action);
      this.alarms.push(a);
      return a;
    };

    alarm(
      'QueueBacklogAge',
      'queue-backlog-age',
      `The oldest reservation request has waited over ${BACKLOG_AGE_THRESHOLD.toSeconds()} s: the worker is falling behind or failing.`,
      props.reservationQueue.metricApproximateAgeOfOldestMessage({ period, statistic: 'Maximum' }),
      BACKLOG_AGE_THRESHOLD.toSeconds(),
      ComparisonOperator.GREATER_THAN_OR_EQUAL_TO_THRESHOLD,
      BACKLOG_EVALUATION_PERIODS,
    );

    alarm(
      'DlqMessages',
      'dlq-messages',
      'Reservation requests failed every retry and are in the dead letter queue.',
      props.reservationDlq.metricApproximateNumberOfMessagesVisible({
        period,
        statistic: 'Maximum',
      }),
      0,
      ComparisonOperator.GREATER_THAN_THRESHOLD,
    );

    // Per-record failures are reported in BatchItemFailures, so they never raise the Lambda
    // Errors metric; the worker's ERROR log lines are counted instead.
    const errorLogs = new MetricFilter(this, 'WorkerErrorLogFilter', {
      logGroup: props.workerLogGroup,
      filterPattern: WORKER_ERROR_LOG_PATTERN,
      metricNamespace: `Pokedrop/${props.stage}`,
      metricName: 'WorkerErrorLogs',
      metricValue: '1',
    });
    alarm(
      'WorkerErrorLogs',
      'worker-error-logs',
      'The reservation worker logged an ERROR (a message failed permanently, or startup failed).',
      errorLogs.metric({ period, statistic: 'Sum' }),
      1,
      ComparisonOperator.GREATER_THAN_OR_EQUAL_TO_THRESHOLD,
    );

    alarm(
      'WorkerCrashes',
      'worker-crashes',
      'A reservation worker invocation crashed, timed out or was throttled.',
      new MathExpression({
        expression: 'FILL(errors, 0) + FILL(throttles, 0)',
        usingMetrics: {
          errors: props.workerFunction.metricErrors({ period, statistic: 'Sum' }),
          throttles: props.workerFunction.metricThrottles({ period, statistic: 'Sum' }),
        },
        period,
        label: 'Worker errors + throttles',
      }),
      1,
      ComparisonOperator.GREATER_THAN_OR_EQUAL_TO_THRESHOLD,
    );
  }
}
