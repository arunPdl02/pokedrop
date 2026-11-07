import * as path from 'path';
import { Duration, RemovalPolicy } from 'aws-cdk-lib';
import { CorsHttpMethod, HttpApi, HttpMethod } from 'aws-cdk-lib/aws-apigatewayv2';
import { HttpJwtAuthorizer } from 'aws-cdk-lib/aws-apigatewayv2-authorizers';
import { HttpLambdaIntegration } from 'aws-cdk-lib/aws-apigatewayv2-integrations';
import { Architecture, Code, Function as LambdaFunction, Runtime } from 'aws-cdk-lib/aws-lambda';
import { Construct } from 'constructs';
import { Auth, FRONTEND_ORIGIN, REQUIRED_SCOPE } from './auth';
import { serviceLogGroup } from './log-group';

export interface ApiProps {
  readonly auth: Auth;
  readonly removalPolicy: RemovalPolicy;
}

/**
 * Directory holding the cross-compiled `bootstrap` binary. `make build-lambda` produces it; it
 * must exist before synth (so before the infra tests, which `make test` takes care of).
 */
const API_CODE_DIR = path.join(__dirname, '..', '..', 'backend', 'build', 'api');

/** HTTP API behind the Cognito JWT authorizer, served by the Go API Lambda. */
export class Api extends Construct {
  public readonly httpApi: HttpApi;
  public readonly function: LambdaFunction;

  constructor(scope: Construct, id: string, props: ApiProps) {
    super(scope, id);

    this.function = new LambdaFunction(this, 'Function', {
      runtime: Runtime.PROVIDED_AL2023,
      architecture: Architecture.ARM_64,
      handler: 'bootstrap',
      code: Code.fromAsset(API_CODE_DIR),
      memorySize: 256,
      timeout: Duration.seconds(10),
      logGroup: serviceLogGroup(this, 'Logs', props.removalPolicy),
    });

    const audience = [props.auth.userPoolClient.userPoolClientId];
    if (props.auth.devClient) {
      audience.push(props.auth.devClient.userPoolClientId);
    }

    // The authorizer and the required scope are API-wide defaults, so a route added later cannot
    // be left open by accident.
    this.httpApi = new HttpApi(this, 'HttpApi', {
      defaultAuthorizer: new HttpJwtAuthorizer(
        'JwtAuthorizer',
        props.auth.userPool.userPoolProviderUrl,
        { jwtAudience: audience },
      ),
      defaultAuthorizationScopes: [REQUIRED_SCOPE],
      corsPreflight: {
        allowOrigins: [FRONTEND_ORIGIN],
        allowMethods: [CorsHttpMethod.GET, CorsHttpMethod.POST, CorsHttpMethod.OPTIONS],
        allowHeaders: ['Authorization', 'Content-Type'],
        maxAge: Duration.hours(1),
      },
    });

    this.httpApi.addRoutes({
      path: '/reservations',
      methods: [HttpMethod.POST],
      integration: new HttpLambdaIntegration('ApiIntegration', this.function),
    });
  }
}
