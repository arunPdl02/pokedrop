import { DefaultStackSynthesizer, Duration, Stack, StackProps, Tags } from 'aws-cdk-lib';
import {
  Effect,
  IOidcProvider,
  OidcProviderNative,
  PolicyStatement,
  Role,
  WebIdentityPrincipal,
} from 'aws-cdk-lib/aws-iam';
import { Construct } from 'constructs';

/** The only repository whose workflows can assume the deploy role. */
export const GITHUB_REPO = 'arunPdl02/pokedrop';

/**
 * How this repository's OIDC tokens start their `sub` claim. GitHub gives repositories immutable
 * subjects, made of the owner and repository IDs instead of their names, so a renamed, deleted and
 * recreated or transferred repository cannot inherit the trust. The name-based form
 * (`repo:arunPdl02/pokedrop`) is therefore NOT what these tokens carry, and a trust policy written
 * for it is refused, which is how the first deploy run failed. Read the real value with
 * `gh api repos/arunPdl02/pokedrop/actions/oidc/customization/sub` (`sub_claim_prefix`).
 */
export const GITHUB_SUBJECT_PREFIX = 'repo:arunPdl02@135916160/pokedrop@1442086654';

/** The only branch. The token's `sub` is `<subject prefix>:ref:refs/heads/<branch>` for a push or a dispatch on it. */
export const DEPLOY_BRANCH = 'main';

export const GITHUB_OIDC_HOST = 'token.actions.githubusercontent.com';
export const GITHUB_OIDC_AUDIENCE = 'sts.amazonaws.com';

export const DEPLOY_ROLE_NAME = 'pokedrop-github-deploy';

/**
 * The CDK bootstrap roles the pipeline may assume, and nothing else: `cdk deploy` assumes `deploy`
 * (starts the CloudFormation change set; CloudFormation then acts through the bootstrap's own
 * execution role), `file-publishing` (uploads the Lambda zips and the site to the assets bucket) and
 * `lookup`. There is no `image-publishing`, because nothing here is a container image.
 */
export const BOOTSTRAP_ROLE_TYPES = ['deploy', 'file-publishing', 'lookup'] as const;

export interface CiStackProps extends StackProps {
  /** Defaults to this repository's subject prefix (GITHUB_SUBJECT_PREFIX). Exposed for tests. */
  readonly subjectPrefix?: string;
  readonly branch?: string;
  /** CDK bootstrap qualifier. The default bootstrap, which this account uses, is `hnb659fds`. */
  readonly qualifier?: string;
  /**
   * ARN of the account's existing GitHub OIDC provider, if there is one. An account can have only
   * one provider per URL, so creating a second fails; pass `-c githubOidcProviderArn=...` then.
   */
  readonly existingProviderArn?: string;
}

/**
 * Lets GitHub Actions deploy `pokedrop-prod` without stored AWS keys: an OIDC identity provider
 * and one role that only the `main` branch of this repository can assume, and that can do nothing
 * except assume the CDK bootstrap roles.
 *
 * It is a stack of its own, created only for `prod`, so deploying prod never edits the role that
 * deploys it, and the account-wide provider outlives the app stack. You deploy it by hand once
 * (`make deploy-ci`); after that the deploy workflow takes over `pokedrop-prod`.
 */
export class CiStack extends Stack {
  public readonly provider: IOidcProvider;
  public readonly deployRole: Role;

  constructor(scope: Construct, id: string, props: CiStackProps = {}) {
    super(scope, id, props);

    const subjectPrefix = props.subjectPrefix ?? GITHUB_SUBJECT_PREFIX;
    const branch = props.branch ?? DEPLOY_BRANCH;
    const qualifier = props.qualifier ?? DefaultStackSynthesizer.DEFAULT_QUALIFIER;

    Tags.of(this).add('project', 'pokedrop');

    this.provider = props.existingProviderArn
      ? OidcProviderNative.fromOidcProviderArn(this, 'GithubProvider', props.existingProviderArn)
      : new OidcProviderNative(this, 'GithubProvider', {
          url: `https://${GITHUB_OIDC_HOST}`,
          clientIds: [GITHUB_OIDC_AUDIENCE],
        });

    this.deployRole = new Role(this, 'DeployRole', {
      roleName: DEPLOY_ROLE_NAME,
      description: `Assumed by GitHub Actions on ${GITHUB_REPO}@${branch} to run cdk deploy; may only assume the CDK bootstrap roles.`,
      maxSessionDuration: Duration.hours(1),
      // Exact matches only: this audience and this one branch of this one repository. A workflow
      // on a pull request, another branch, a tag, an environment or a fork has a different `sub`
      // and is refused by STS before any permission is looked at.
      assumedBy: new WebIdentityPrincipal(this.provider.oidcProviderArn, {
        StringEquals: {
          [`${GITHUB_OIDC_HOST}:aud`]: GITHUB_OIDC_AUDIENCE,
          [`${GITHUB_OIDC_HOST}:sub`]: `${subjectPrefix}:ref:refs/heads/${branch}`,
        },
      }),
    });

    this.deployRole.addToPolicy(
      new PolicyStatement({
        sid: 'AssumeCdkBootstrapRoles',
        effect: Effect.ALLOW,
        actions: ['sts:AssumeRole'],
        resources: BOOTSTRAP_ROLE_TYPES.map(
          (type) =>
            `arn:${this.partition}:iam::${this.account}:role/cdk-${qualifier}-${type}-role-${this.account}-${this.region}`,
        ),
      }),
    );
  }
}
