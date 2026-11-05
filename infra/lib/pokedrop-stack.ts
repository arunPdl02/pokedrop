import { CfnOutput, RemovalPolicy, Stack, StackProps, Tags } from 'aws-cdk-lib';
import { AttributeType, BillingMode, Table } from 'aws-cdk-lib/aws-dynamodb';
import { Construct } from 'constructs';

export interface PokedropStackProps extends StackProps {
  /** Deployment stage, e.g. `dev` or `test`. Everything except `prod` is destroyable. */
  readonly stage: string;
}

/** Key and attribute names mirror backend/internal/storage/schema.go. */
export class PokedropStack extends Stack {
  public readonly inventoryTable: Table;
  public readonly reservationsTable: Table;

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

    new CfnOutput(this, 'InventoryTableName', { value: this.inventoryTable.tableName });
    new CfnOutput(this, 'ReservationsTableName', { value: this.reservationsTable.tableName });
  }
}
