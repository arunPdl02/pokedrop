import * as path from 'path';
import { RemovalPolicy } from 'aws-cdk-lib';
import {
  CachePolicy,
  Distribution,
  PriceClass,
  ResponseHeadersPolicy,
  ViewerProtocolPolicy,
} from 'aws-cdk-lib/aws-cloudfront';
import { S3BucketOrigin } from 'aws-cdk-lib/aws-cloudfront-origins';
import { BlockPublicAccess, Bucket, BucketEncryption } from 'aws-cdk-lib/aws-s3';
import { BucketDeployment, Source } from 'aws-cdk-lib/aws-s3-deployment';
import { Construct } from 'constructs';

/**
 * Directory holding the built SPA. `make build-frontend` produces it; it must exist before synth
 * (so before the infra tests, which `make test` takes care of).
 */
const FRONTEND_DIST_DIR = path.join(__dirname, '..', '..', 'frontend', 'dist');

/** What the SPA reads from /config.json (mirrors frontend/src/config.ts, `RuntimeConfig`). */
export interface FrontendConfig {
  readonly apiUrl: string;
  readonly clientId: string;
  /** Hosted UI host name without a scheme. */
  readonly hostedUiDomain: string;
}

/**
 * The public demo site: a private S3 bucket behind CloudFront (origin access control), so the
 * bucket is never reachable directly. Created for the `prod` stage only.
 *
 * Two steps because of a dependency order: the distribution's URL must exist before Cognito and the
 * API (which allow it as an origin), and the config the site reads needs those two in turn. The
 * distribution depends only on the bucket, so there is no cycle. Construct it first, pass
 * `origin` to Auth and Api, then call `deploy` with their values.
 */
export class Frontend extends Construct {
  public readonly bucket: Bucket;
  public readonly distribution: Distribution;
  /** `https://<id>.cloudfront.net`: the sign-in redirect and CORS origin. No trailing slash. */
  public readonly origin: string;

  constructor(scope: Construct, id: string) {
    super(scope, id);

    // The contents are rebuilt from the repo on every deploy, so nothing here is worth keeping:
    // the bucket is emptied and deleted with the stack (unlike the tables and the user pool).
    this.bucket = new Bucket(this, 'Bucket', {
      blockPublicAccess: BlockPublicAccess.BLOCK_ALL,
      encryption: BucketEncryption.S3_MANAGED,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.DESTROY,
      autoDeleteObjects: true,
    });

    this.distribution = new Distribution(this, 'Distribution', {
      comment: 'PokeDrop demo frontend',
      defaultRootObject: 'index.html',
      defaultBehavior: {
        origin: S3BucketOrigin.withOriginAccessControl(this.bucket),
        viewerProtocolPolicy: ViewerProtocolPolicy.REDIRECT_TO_HTTPS,
        cachePolicy: CachePolicy.CACHING_OPTIMIZED,
        responseHeadersPolicy: ResponseHeadersPolicy.SECURITY_HEADERS,
        compress: true,
      },
      // No minimumProtocolVersion: CloudFront ignores it (and CDK drops it) for the default
      // *.cloudfront.net certificate, so TLS 1.2 as a floor needs a custom domain and certificate.
      priceClass: PriceClass.PRICE_CLASS_100,
    });

    this.origin = `https://${this.distribution.distributionDomainName}`;
  }

  /**
   * Uploads the built site and a config.json made from this stack's own values, then invalidates
   * the cache so the new files are served at once. The config source comes last, so it replaces
   * any config.json that `make frontend-config` left in frontend/public.
   */
  public deploy(config: FrontendConfig): void {
    new BucketDeployment(this, 'Deployment', {
      sources: [
        Source.asset(FRONTEND_DIST_DIR),
        Source.jsonData('config.json', {
          apiUrl: config.apiUrl,
          clientId: config.clientId,
          hostedUiDomain: config.hostedUiDomain,
        }),
      ],
      destinationBucket: this.bucket,
      distribution: this.distribution,
      distributionPaths: ['/*'],
    });
  }
}
