// Command muster-deploy stands muster's optional hosted backend up in an AWS
// account: one DynamoDB table, one Lambda, and the HTTP API in front of it.
//
// It is a SEPARATE binary from muster on purpose. Deployment needs the AWS
// SDK and AWS credentials; devices need neither, and keeping the two apart is
// what guarantees that — `go list -deps ./cmd/muster` has no AWS package in
// it, and `just verify` builds both configurations so the edge cannot widen
// by accident. Operators who never self-host never download this.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/subaud-tools/muster/internal/deploy"
	"github.com/subaud-tools/muster/internal/paths"
	"github.com/subaud-tools/muster/internal/version"
)

func main() { os.Exit(run()) }

func run() int {
	fs := flag.NewFlagSet("muster-deploy", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
	}
	var (
		stack  = fs.String("stack", "muster", "CloudFormation stack name")
		region = fs.String("region", "", "AWS region (default: AWS_REGION or your profile's region)")
		bucket = fs.String("bucket", "", "artifact bucket (default: muster-artifacts-<account>, created if absent)")
		tag    = fs.String("tag", "", "release tag to fetch the Lambda artifact from (default: this binary's version)")
		zip    = fs.String("zip", "", "local Lambda zip instead of downloading a release artifact")
		token  = fs.String("token", "", "bearer token (default: generate on first deploy, keep the existing one on update)")
		tokFil = fs.String("token-file", "", "where to write a generated token (default: <MUSTER_HOME>/remote-token)")
		wait   = fs.Duration("wait", 30*time.Minute, "how long to wait for the stack to settle")
		domain = fs.String("domain", "", "custom hostname for the bus, e.g. muster.example.com (keeps the URL stable across stack recreation)")
		zone   = fs.String("hosted-zone", "", "Route53 hosted zone id for -domain; lets the stack create and validate the certificate")
		cert   = fs.String("cert", "", "existing ACM certificate ARN for -domain, in this region, instead of creating one")
		rmDom  = fs.Bool("remove-domain", false, "tear down the custom domain, its certificate and its DNS record (omitting -domain alone leaves them alone)")
		join   = fs.Bool("join", false, "deploy nothing; print setup instructions (including the token) for adding another device")
		showV  = fs.Bool("version", false, "print version and exit")
	)
	// A bare invocation prints usage instead of deploying. Every other flag has
	// a defensible default — region and account resolve from the ambient AWS
	// config — so `muster-deploy` with no arguments would quietly create real
	// infrastructure in whatever account happened to be configured. That is
	// too easy to do by accident for something installable in one line, and it
	// is what the installer's smoke test would otherwise trigger. Exit 2 is
	// muster's own convention for "usage, not failure".
	if len(os.Args) == 1 {
		fs.Usage()
		return 2
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *showV {
		fmt.Println("muster-deploy " + version.Version() + " (" + version.Commit() + ")")
		return 0
	}

	// An unstamped build (plain `go build`, no ldflags) has no release to
	// infer, so it must be told one rather than guessing a tag that does not
	// exist and failing later at a 404.
	if *tag == "" && *zip == "" {
		if v := version.Version(); v != "" && v != "dev" {
			*tag = deploy.ReleaseTag(v)
		}
	}
	if *tokFil == "" {
		*tokFil = filepath.Join(paths.Home(), "remote-token")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// -join deploys nothing. It is the answer to "I have a bus, how do I add
	// my other laptop" — a question the deploy path cannot answer, because
	// moving the token to a machine with no AWS credentials is necessarily a
	// human step.
	if *join {
		info, err := deploy.Join(ctx, deploy.Options{
			Stack: *stack, Region: *region, TokenFile: *tokFil,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster-deploy:", err)
			return 1
		}
		info.WriteInstructions(os.Stdout)
		return 0
	}

	res, err := deploy.Run(ctx, deploy.Options{
		Stack: *stack, Region: *region, Bucket: *bucket, Tag: *tag,
		ZipPath: *zip, Token: *token, TokenFile: *tokFil, Wait: *wait, Out: os.Stdout,
		Domain: *domain, HostedZoneID: *zone, CertARN: *cert, RemoveDomain: *rmDom,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "muster-deploy:", err)
		return 1
	}
	report(res)
	return 0
}

func report(r *deploy.Result) {
	verb := "updated"
	if r.Created {
		verb = "deployed"
	}
	fmt.Printf("\nmuster hosted backend %s.\n\n", verb)
	fmt.Printf("  endpoint  %s\n", r.Endpoint)
	fmt.Printf("  table     %s\n", r.Table)
	fmt.Printf("  function  %s\n", r.Function)
	if r.TokenWritten != "" {
		fmt.Printf("  token     written to %s (mode 0600)\n", r.TokenWritten)
	}
	fmt.Printf("\nOn every device that should join this bus:\n\n")
	fmt.Printf("  export MUSTER_BACKEND=remote\n")
	fmt.Printf("  export MUSTER_REMOTE_URL=%s\n\n", r.Endpoint)
	if r.TokenWritten != "" {
		fmt.Printf("The token is not printed here on purpose — it is the only thing\n")
		fmt.Printf("protecting the bus, and printed secrets end up in scrollback.\n\n")
		fmt.Printf("To add another device, run:  muster-deploy -join\n")
	}
}

const usage = `muster-deploy — stand up muster's hosted backend in an AWS account.

Creates (or updates) one CloudFormation stack: a DynamoDB table, a Lambda
running the same daemon code the unix socket serves, and an HTTP API in front
of it that authenticates a bearer token. Uses your ambient AWS credentials —
AWS_PROFILE and AWS_REGION work as they do for any AWS command.

On a first deploy it generates a bearer token and writes it to
<MUSTER_HOME>/remote-token with mode 0600, rather than printing it. On an
update it keeps the token already in the stack, so re-deploying never rotates
your fleet's credential by accident.

Usage:
  muster-deploy [flags]

Flags:
`
