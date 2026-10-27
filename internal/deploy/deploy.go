// Package deploy stands the hosted backend up in an AWS account: it uploads
// the Lambda artifact, then creates or updates the CloudFormation stack that
// owns the DynamoDB table, the function, and the HTTP API in front of it.
//
// It exists because the manual path has a step that is pure friction —
// CloudFormation cannot fetch function code over HTTPS, so the zip has to be
// staged in S3 first, which means an operator deploying a personal
// coordination bus is asked to create a bucket before they can begin. Nothing
// about that step is a decision they should have to make, so this makes it.
//
// This package and cmd/muster-deploy are the ONLY non-lambda packages
// permitted to import the AWS SDK. That is not a contradiction of the rule in
// CLAUDE.md: the rule protects the DEVICE binary (cmd/muster), and deployment
// is inherently an operation on an AWS account by someone who already has
// credentials for it. What must stay true is that no import edge runs from
// cmd/muster to here — devices reach the hosted bus over plain HTTPS with a
// bearer token and need no AWS anything.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	cfn "github.com/subaud-tools/muster/contrib/cloudformation"
)

// Options configures one deployment. Zero values are meaningful defaults
// wherever a default is defensible; the two that are not — which account and
// which region — come from the standard AWS credential chain, so the tool
// obeys AWS_PROFILE and AWS_REGION like every other AWS command does.
type Options struct {
	Stack     string // CloudFormation stack name
	Region    string // "" defers to the AWS config chain
	Bucket    string // "" derives muster-artifacts-<account>
	Tag       string // release tag to fetch the Lambda zip from
	ZipPath   string // local zip instead of a release download
	Token     string // "" generates one (new stack) or keeps the current one (update)
	TokenFile string // where to write a freshly generated token
	Wait      time.Duration
	Out       io.Writer

	// Optional custom domain. Empty Domain leaves the generated execute-api
	// endpoint in place and creates no certificate, domain, or DNS.
	Domain       string
	HostedZoneID string
	CertARN      string
	// RemoveDomain tears the custom domain down. Explicit because the absence
	// of -domain means "leave it alone", not "delete it".
	RemoveDomain bool
}

// validateDomain rejects the one combination that fails badly rather than
// loudly: a domain with neither a hosted zone to validate a certificate
// through nor a certificate already in hand. CloudFormation's behaviour there
// is to sit in CREATE_IN_PROGRESS until it times out — by default hours later
// — with nothing in the events explaining why, so catching it here before a
// single API call is worth more than any error message the stack could give.
func (o Options) validateDomain() error {
	if o.RemoveDomain && (o.Domain != "" || o.HostedZoneID != "" || o.CertARN != "") {
		return errors.New("-remove-domain cannot be combined with -domain, -hosted-zone or -cert")
	}
	if o.Domain == "" {
		if o.HostedZoneID != "" || o.CertARN != "" {
			return errors.New("-hosted-zone and -cert do nothing without -domain")
		}
		return nil
	}
	if o.HostedZoneID == "" && o.CertARN == "" {
		return fmt.Errorf("-domain %s needs either -hosted-zone (to create and validate a certificate) "+
			"or -cert (an ACM certificate you have already validated, in this stack's region)", o.Domain)
	}
	return nil
}

// Result is what an operator needs after a successful deploy.
type Result struct {
	Endpoint     string
	Table        string
	Function     string
	TokenWritten string // path a newly generated token was written to, "" if none
	Token        string // set only when newly generated
	Created      bool   // true for a create, false for an update
}

// Run performs the deployment end to end.
func Run(ctx context.Context, o Options) (*Result, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Stack == "" {
		o.Stack = "muster"
	}
	if o.Wait <= 0 {
		o.Wait = 30 * time.Minute
	}
	if err := o.validateDomain(); err != nil {
		return nil, err
	}

	var loadOpts []func(*config.LoadOptions) error
	if o.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(o.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("no AWS region: pass -region, or set AWS_REGION / a region in your profile")
	}

	ident, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("resolve AWS identity (are your credentials valid?): %w", err)
	}
	account := aws.ToString(ident.Account)
	if o.Bucket == "" {
		o.Bucket = BucketName(account)
	}
	o.logf("account %s · region %s · stack %s\n", account, cfg.Region, o.Stack)

	cfnAPI := cloudformation.NewFromConfig(cfg)
	exists, err := stackExists(ctx, cfnAPI, o.Stack)
	if err != nil {
		return nil, err
	}

	// A token is required to CREATE and must not be demanded to UPDATE:
	// asking for it on every update would mean either keeping it somewhere
	// re-readable or rotating it by accident, and CloudFormation can carry
	// the stored value forward instead.
	res := &Result{Created: !exists}
	switch {
	case o.Token != "":
	case !exists:
		if o.Token, err = GenerateToken(); err != nil {
			return nil, err
		}
		res.Token = o.Token
	}

	s3API := s3.NewFromConfig(cfg)
	if err := ensureBucket(ctx, s3API, o.Bucket, cfg.Region, o.Out); err != nil {
		return nil, err
	}

	key, err := uploadArtifact(ctx, s3API, o)
	if err != nil {
		return nil, err
	}

	if err := applyStack(ctx, cfnAPI, o, key, exists); err != nil {
		return nil, err
	}

	outs, err := stackOutputs(ctx, cfnAPI, o.Stack)
	if err != nil {
		return nil, err
	}
	res.Endpoint = outs["MusterUrl"]
	res.Table = outs["TableName"]
	res.Function = outs["FunctionName"]

	// The token is written to disk rather than printed. It is the whole
	// security of the bus, and a printed secret lands in scrollback, in tmux
	// history, and — since muster runs alongside coding agents that read
	// their own terminals — plausibly in a model's context.
	if res.Token != "" && o.TokenFile != "" {
		if err := WriteToken(o.TokenFile, res.Token); err != nil {
			return nil, fmt.Errorf("write token: %w", err)
		}
		res.TokenWritten = o.TokenFile
	}
	return res, nil
}

// logf and logln write progress to o.Out. Progress output is advisory: a
// deploy that succeeded must not be reported as failed because a pipe closed,
// so the write error is discarded deliberately rather than by omission.
func (o Options) logf(format string, a ...any) { logfTo(o.Out, format, a...) }
func (o Options) logln(a ...any)               { _, _ = fmt.Fprintln(o.Out, a...) }

func logfTo(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

// stackExists reports whether the named stack is present. A ValidationError
// from DescribeStacks is how CloudFormation says "no such stack" — there is
// no typed not-found error for it — so the string match is the API, not a
// shortcut.
func stackExists(ctx context.Context, api *cloudformation.Client, name string) (bool, error) {
	_, err := api.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(name)})
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "does not exist") {
		return false, nil
	}
	return false, fmt.Errorf("describe stack %s: %w", name, err)
}

// ensureBucket creates the artifact bucket when it is missing. This is the
// step the tool exists to remove: CloudFormation cannot fetch function code
// over HTTPS, so a bucket must exist before a first deploy can happen at all.
func ensureBucket(ctx context.Context, api *s3.Client, bucket, region string, out io.Writer) error {
	_, err := api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return nil
	}
	in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	// us-east-1 is the one region that must NOT carry a LocationConstraint;
	// sending it there is an InvalidLocationConstraint error rather than a
	// no-op, which is why this is a special case and not a uniform field.
	if region != "us-east-1" {
		in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}
	if _, err := api.CreateBucket(ctx, in); err != nil {
		return fmt.Errorf("create artifact bucket %s: %w", bucket, err)
	}
	logfTo(out, "created artifact bucket %s\n", bucket)
	return nil
}

// uploadArtifact puts the Lambda zip in S3 and returns its key.
func uploadArtifact(ctx context.Context, api *s3.Client, o Options) (string, error) {
	path, key := o.ZipPath, ""
	if path == "" {
		if o.Tag == "" {
			return "", errors.New("no Lambda artifact: pass -zip <path>, or -tag <release> to download one " +
				"(an unstamped dev build cannot infer its own release)")
		}
		o.logf("downloading %s\n", ArtifactURL(DLBase, o.Tag))
		var cleanup func()
		var err error
		path, cleanup, err = fetchArtifact(DLBase, o.Tag)
		if err != nil {
			return "", err
		}
		defer cleanup()
		key = ArtifactName(o.Tag)
	} else {
		// A local zip still gets a version-stamped key when a tag is known,
		// so that re-deploying a rebuilt local artifact under the same tag
		// does not silently leave the old code running.
		key = ArtifactName(o.Tag)
		if o.Tag == "" {
			key = ArtifactName(fmt.Sprintf("local-%d", time.Now().Unix()))
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open artifact: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(o.Bucket), Key: aws.String(key), Body: f,
	}); err != nil {
		return "", fmt.Errorf("upload artifact to s3://%s/%s: %w", o.Bucket, key, err)
	}
	o.logf("uploaded s3://%s/%s\n", o.Bucket, key)
	return key, nil
}

// domainParams builds the three custom-domain parameters.
//
// Absent flags mean "leave it alone", never "delete it". Sending them empty on
// every deploy would make a routine redeploy — new function code, no other
// flags — destroy the custom domain, its certificate and its DNS record, and
// every device pointed at that hostname would stop resolving. The bearer token
// already follows this rule for the same reason; getting it backwards here
// would have been far more destructive.
func domainParams(o Options, exists bool) []cfntypes.Parameter {
	out := make([]cfntypes.Parameter, 0, 3)
	for _, kv := range []struct{ k, v string }{
		{"DomainName", o.Domain}, {"HostedZoneId", o.HostedZoneID}, {"CertificateArn", o.CertARN},
	} {
		switch {
		case !o.RemoveDomain && kv.v != "":
			out = append(out, cfntypes.Parameter{ParameterKey: aws.String(kv.k), ParameterValue: aws.String(kv.v)})
		case !o.RemoveDomain && exists:
			// Nothing said about it on an update: keep the stored value.
			out = append(out, cfntypes.Parameter{ParameterKey: aws.String(kv.k), UsePreviousValue: aws.Bool(true)})
		default:
			// An explicit removal, or a CREATE, which has no previous value.
			out = append(out, cfntypes.Parameter{ParameterKey: aws.String(kv.k), ParameterValue: aws.String("")})
		}
	}
	return out
}

// applyStack creates or updates the stack and waits for it to settle.
func applyStack(ctx context.Context, api *cloudformation.Client, o Options, key string, exists bool) error {
	params := []cfntypes.Parameter{
		{ParameterKey: aws.String("CodeS3Bucket"), ParameterValue: aws.String(o.Bucket)},
		{ParameterKey: aws.String("CodeS3Key"), ParameterValue: aws.String(key)},
	}
	params = append(params, domainParams(o, exists)...)
	if o.Token != "" {
		params = append(params, cfntypes.Parameter{
			ParameterKey: aws.String("MusterToken"), ParameterValue: aws.String(o.Token),
		})
	} else {
		params = append(params, cfntypes.Parameter{
			ParameterKey: aws.String("MusterToken"), UsePreviousValue: aws.Bool(true),
		})
	}
	caps := []cfntypes.Capability{cfntypes.CapabilityCapabilityIam}

	if !exists {
		o.logln("creating stack…")
		if _, err := api.CreateStack(ctx, &cloudformation.CreateStackInput{
			StackName: aws.String(o.Stack), TemplateBody: aws.String(cfn.Template),
			Parameters: params, Capabilities: caps,
		}); err != nil {
			return fmt.Errorf("create stack: %w", err)
		}
		w := cloudformation.NewStackCreateCompleteWaiter(api)
		if err := w.Wait(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(o.Stack)}, o.Wait); err != nil {
			return fmt.Errorf("stack create did not complete: %w", err)
		}
		return nil
	}

	o.logln("updating stack…")
	if _, err := api.UpdateStack(ctx, &cloudformation.UpdateStackInput{
		StackName: aws.String(o.Stack), TemplateBody: aws.String(cfn.Template),
		Parameters: params, Capabilities: caps,
	}); err != nil {
		// "No updates are to be performed" is CloudFormation reporting that
		// the deploy was a no-op, which is a SUCCESS: re-running a deploy
		// that changes nothing must not fail a script.
		if strings.Contains(err.Error(), "No updates are to be performed") {
			o.logln("stack already up to date")
			return nil
		}
		return fmt.Errorf("update stack: %w", err)
	}
	w := cloudformation.NewStackUpdateCompleteWaiter(api)
	if err := w.Wait(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(o.Stack)}, o.Wait); err != nil {
		return fmt.Errorf("stack update did not complete: %w", err)
	}
	return nil
}

// stackOutputs reads the finished stack's outputs into a map.
func stackOutputs(ctx context.Context, api *cloudformation.Client, name string) (map[string]string, error) {
	out, err := api.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(name)})
	if err != nil {
		return nil, fmt.Errorf("describe stack %s: %w", name, err)
	}
	if len(out.Stacks) == 0 {
		return nil, fmt.Errorf("stack %s vanished between deploy and describe", name)
	}
	m := make(map[string]string, len(out.Stacks[0].Outputs))
	for _, o := range out.Stacks[0].Outputs {
		m[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}
	return m, nil
}
