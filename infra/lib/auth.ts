import { Aws, Fn, RemovalPolicy } from 'aws-cdk-lib';
import {
  AccountRecovery,
  OAuthScope,
  UserPool,
  UserPoolClient,
  UserPoolDomain,
} from 'aws-cdk-lib/aws-cognito';
import { Construct } from 'constructs';

/** The only origin the frontend runs from until Phase 7 adds the CloudFront URL. */
export const FRONTEND_ORIGIN = 'http://localhost:5173';

/**
 * Scope every API route requires. It is the one scope carried by access tokens from both the
 * hosted UI (when requested) and ADMIN_USER_PASSWORD_AUTH, so one rule covers both clients.
 */
export const REQUIRED_SCOPE = 'aws.cognito.signin.user.admin';

export interface AuthProps {
  /** Deployment stage. The dev-only client is never created for `prod`. */
  readonly stage: string;
  readonly removalPolicy: RemovalPolicy;
}

/** Trainer sign-in: user pool, hosted UI domain, the PKCE frontend client and a dev-only client. */
export class Auth extends Construct {
  public readonly userPool: UserPool;
  public readonly domain: UserPoolDomain;
  /** Authorization code + PKCE client used by the frontend. */
  public readonly userPoolClient: UserPoolClient;
  /** ADMIN_USER_PASSWORD_AUTH client for `make token`. Undefined on `prod`. */
  public readonly devClient?: UserPoolClient;

  constructor(scope: Construct, id: string, props: AuthProps) {
    super(scope, id);

    this.userPool = new UserPool(this, 'UserPool', {
      signInAliases: { email: true },
      autoVerify: { email: true },
      selfSignUpEnabled: true,
      accountRecovery: AccountRecovery.EMAIL_ONLY,
      removalPolicy: props.removalPolicy,
    });

    // Prefix must be unique per region. The stack ID's GUID suffix keeps it unique without putting
    // the account ID in a repository that will be public. It changes if the stack is recreated.
    const uniqueSuffix = Fn.select(4, Fn.split('-', Fn.select(2, Fn.split('/', Aws.STACK_ID))));
    this.domain = this.userPool.addDomain('HostedUi', {
      cognitoDomain: { domainPrefix: `pokedrop-${props.stage}-${uniqueSuffix}` },
    });

    this.userPoolClient = this.userPool.addClient('FrontendClient', {
      generateSecret: false,
      oAuth: {
        flows: { authorizationCodeGrant: true },
        scopes: [OAuthScope.OPENID, OAuthScope.EMAIL, OAuthScope.COGNITO_ADMIN],
        callbackUrls: [FRONTEND_ORIGIN],
        logoutUrls: [FRONTEND_ORIGIN],
      },
      preventUserExistenceErrors: true,
    });

    if (props.stage !== 'prod') {
      this.devClient = this.userPool.addClient('DevClient', {
        generateSecret: false,
        authFlows: { adminUserPassword: true },
        disableOAuth: true,
        preventUserExistenceErrors: true,
      });
    }
  }
}
