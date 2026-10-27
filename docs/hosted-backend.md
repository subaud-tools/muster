# The hosted backend

muster normally keeps its state in a SQLite file that one daemon owns on one
machine. That is the default and it is not going away. This document describes
the optional alternative: a bus that lives in your own AWS account, so agents
running on your laptop and agents running on your desktop are on the same
roster and can address each other.

If you would rather have a coding agent walk you through this, the
`.claude/skills/muster-hosted-backend` skill covers the same ground in the form
an agent can act on. One instruction there is worth knowing about even if you
never read it: the skill tells the agent never to print your bearer token, and
never to run `muster-deploy -join` itself, because that command's output
contains the token and would put a live credential in the agent's transcript.

## What this is, and what it is not

This is a stack you deploy into an AWS account you control. It is one DynamoDB
table, one Lambda function, and one HTTPS endpoint in front of that function.
Nobody else's data passes through it, and there is no service operated by
anyone else that you are signing up for. The deliverable is a CloudFormation
template and these instructions.

It is built for a single operator with several machines. There is no account
model, no user model, and no multi-tenancy — everyone who holds the token is
the same person as far as the bus is concerned.

Your devices do not need AWS credentials, an AWS profile, a region, or the AWS
SDK. That is the whole reason the Lambda function exists rather than having
each device talk to DynamoDB directly: DynamoDB authenticates with SigV4 and
has no bearer-token equivalent, so a device talking to it would need real AWS
credentials on disk. A Lambda function can authenticate however it likes, and
this one checks a shared bearer token.

The trade that makes that possible is stated plainly in the next section, and
you should read it before you deploy anything.

## The security model, in full

The function's HTTPS endpoint is an API Gateway HTTP API whose `$default` route
carries no authorizer. That means AWS does not authenticate callers at its
edge. The URL is reachable by anyone on the internet who knows the hostname,
and the bearer token checked inside the function is the only thing between a
stranger and your entire bus — every message anyone has sent, and the ability
to post as any agent on it.

Three consequences follow, and none of them are hypothetical:

**Token entropy is what defends you.** The handler has no lockout and no
per-caller memory; what bounds an online brute force is the API's route
throttling, which the template sets to 20 requests per second by default. That
turns guessing into a rate-limited attack rather than an unbounded one, but 20
guesses a second still adds up over months, so the token must be genuinely
unguessable. Generate it with a CSPRNG and never by hand:

```sh
openssl rand -base64 32
```

The CloudFormation template enforces a 32-character minimum on the token
parameter. That rejects obviously weak values; it cannot rescue a long but
predictable one. Use the command above.

**Do not count on the URL being secret.** An HTTP API endpoint is
`https://<api-id>.execute-api.<region>.amazonaws.com`, where the api-id is ten
characters — real obscurity, but a good deal less than a Lambda Function URL's
32-character subdomain. It is still worth not pasting into an issue, a commit,
or an agent transcript, but treat it as one fewer thing an attacker has to
guess rather than as a second credential.

**A leaked token grants everything until you rotate it.** There is one token
shared by every device, so there is no per-device attribution and no way to
revoke a single machine without rotating all of them. Rotation is documented
below and is designed to be doable without downtime, which is the point — a
rotation procedure that breaks every device at once is a rotation procedure
nobody ever runs.

## Before you start

You need an AWS account, credentials on the machine you deploy from, and a
region picked. Those credentials need rather more than IAM: CloudFormation to
run the stack, IAM to create the execution role, Lambda for the function and
its invoke permission, API Gateway for the HTTP API and its stage, DynamoDB to
create the table and set its TTL, CloudWatch Logs for both log groups, and S3
to stage the code. Adding a custom domain also needs ACM and, if you let the
stack manage DNS, Route53. An administrator identity covers all of it; a
scoped-down one that only grants IAM will fail partway through the stack.

Everything below assumes `us-east-1`; substitute freely, but keep the S3 bucket
and any ACM certificate in the same region as the stack.

Note that this is about the machine you *deploy* from. Devices that merely join
the bus need none of it — no credentials, no profile, no region, no SDK.

Devices need **muster v0.10.0 or newer** — see the device setup below for why
that is not a soft requirement.

## Deploying the stack

### The short way

Install `muster-deploy` — it does not come with `muster`, because it is needed
on one machine, once:

```sh
curl -fsSL https://muster.tools/install.sh | sh -s -- --with-deploy
```

(or grab `muster-deploy_<os>_<arch>.tar.gz` from
`https://muster.tools/dl/muster/<version>/` by hand). Then:

```sh
muster-deploy --region us-east-1
```

That is the whole deployment. It resolves your account from the ambient AWS
credentials (`AWS_PROFILE` and `AWS_REGION` work as they do for any AWS
command), creates an artifact bucket if you have none, downloads the Lambda
zip matching its own version from muster.tools/dl and checks it against the
`.sha256` published beside it, uploads it, creates the stack, waits, and
prints the endpoint.

On a **first** deploy it also generates a bearer token and writes it to
`<MUSTER_HOME>/remote-token` with mode 0600 — it does not print it. That is
deliberate: the token is the only thing protecting the bus, and a printed
secret ends up in scrollback, in tmux history, and plausibly in the context of
a coding agent sharing your terminal. On an **update** it keeps the token
already in the stack, so re-running it never rotates your fleet's credential
by accident.

`muster-deploy` is a separate download from `muster` on purpose. It links the
AWS SDK because talking to CloudFormation and S3 is its entire job, and
bundling it into the device tarball would put AWS code in the archive every
machine unpacks. Devices need no AWS anything.

Useful flags: `-stack` to name the stack something other than `muster`,
`-bucket` to use an artifact bucket you already have, `-tag` to deploy a
release other than the tool's own version, and `-zip` to deploy a locally
built artifact instead of a downloaded one.

### A custom domain, and why you probably want one

By default the endpoint is `https://<api-id>.execute-api.<region>.amazonaws.com`,
where `api-id` is generated when the API is created. That is fine until the day
you delete and recreate the stack — at which point you get a **different** URL
and every device on the bus stops working until you reconfigure it. A custom
domain is the only thing that makes `MUSTER_REMOTE_URL` outlive the stack.

If your domain's DNS is in Route53 in the same account, one flag pair does
everything — certificate, validation, domain, mapping, and the DNS record:

```sh
muster-deploy --region us-east-1 \
  --domain muster.example.com \
  --hosted-zone Z0123456789ABCDEFGHIJ
```

If your DNS lives elsewhere, validate an ACM certificate yourself **in the same
region as the stack** and pass it instead, then point your own DNS at the
`CustomDomainTarget` output:

```sh
muster-deploy --region us-east-1 \
  --domain muster.example.com \
  --cert arn:aws:acm:us-east-1:123456789012:certificate/…
```

The regional detail matters and catches people: an HTTP API's custom domain is
a *regional* endpoint, so the certificate must live in the API's region. The
"certificates must be in us-east-1" rule you may be remembering is a CloudFront
rule and does not apply here.

`muster-deploy` refuses `--domain` without either `--hosted-zone` or `--cert`
before it makes a single AWS call. That combination is the worst failure this
stack can produce: ACM would wait for a DNS validation record nothing is going
to publish, so CloudFormation sits in `CREATE_IN_PROGRESS` for hours and then
times out, with nothing in the stack events explaining why.

The generated `execute-api` endpoint keeps working alongside the custom domain,
and that is useful rather than untidy — if the custom domain stops answering,
trying the generated one separates a DNS or certificate problem from a muster
one.

Skip to [Setting up a device](#setting-up-a-device) — the rest of this section
is the manual equivalent.

### The manual way

Use this if you would rather see every step, or if you need to change something
`muster-deploy` does not expose. You need the release's Lambda zip,
`muster-lambda-arm64-<tag>.zip`, from `https://muster.tools/dl/muster/<version>/`
(`<version>` is the tag without its `v`; check it against the
`muster-lambda-arm64-<tag>.zip.sha256` beside it), and
`contrib/cloudformation/muster-backend.yaml` from this repository — the same
file `muster-deploy` embeds, so both paths deploy identical bytes.

**1. Generate the token and keep it somewhere you can paste from.** You will
need it once now and once per device.

```sh
openssl rand -base64 32
```

**2. Stage the function code in S3.** CloudFormation cannot fetch Lambda code
over HTTPS, so the zip has to sit in a bucket in the same region as the stack.
Any bucket will do; if you do not have one:

```sh
aws s3 mb s3://my-muster-artifacts --region us-east-1
aws s3 cp muster-lambda-arm64-<tag>.zip s3://my-muster-artifacts/
```

S3 bucket names are globally unique across every AWS account, so
`my-muster-artifacts` is almost certainly taken and `mb` will tell you so.
Pick your own name — suffixing your account id is the usual trick — and
substitute it everywhere `my-muster-artifacts` appears below.

Keep the version in the object key. CloudFormation only redeploys Lambda code
when the S3 key or object version *changes*, so if you overwrite one fixed key
and re-run `deploy`, the stack reports no changes and the old binary keeps
running with nothing anywhere saying so.

**3. Deploy.**

```sh
aws cloudformation deploy \
  --template-file contrib/cloudformation/muster-backend.yaml \
  --stack-name muster \
  --capabilities CAPABILITY_IAM \
  --region us-east-1 \
  --parameter-overrides \
      MusterToken="<the token from step 1>" \
      CodeS3Bucket=my-muster-artifacts \
      CodeS3Key=muster-lambda-arm64-<tag>.zip
```

The token parameter is declared `NoEcho`, so it will not appear in stack
events, the console, or `describe-stacks` output. It is still visible in your
shell history — use your shell's leading-space suppression or read it from a
file if that matters to you.

**4. Read the endpoint out of the stack outputs.**

```sh
aws cloudformation describe-stacks --stack-name muster --region us-east-1 \
  --query 'Stacks[0].Outputs[?OutputKey==`MusterUrl`].OutputValue' --output text
```

That URL is what goes in `MUSTER_REMOTE_URL` on every device.

## Setting up a device

Do this on each machine that should join the bus.

### Name the machine (recommended, not required)

```sh
muster device work-laptop
```

You no longer have to run this before registering anything. The first time a
machine registers *any* agent, muster adopts a device name automatically from
its hostname reduced to lowercase letters, digits and dashes — `courts-
macbook-pro-2` — and pins it to `<MUSTER_HOME>/device-name` so it survives
reboots and later hostname changes. That adopted name already does the job
that matters most: **every alias this machine mints — derived, typed, or
allocated — is seeded with it**, so two machines with the same repos checked
out (identical tmux session names, identical directory basenames) can never
silently take over each other's roster row, inbox and read-state included.
Registering is an upsert and the roster row *is* the identity, so without
seeding, the second machine to register would take the first one's row —
and since session hooks re-register on every start, the two machines would
trade it back and forth with mail following whoever went last, silently.

Running `muster device work-laptop` yourself doesn't add that protection —
you already have it — it swaps the auto-adopted hostname for something a
human would actually say: *"message the ci-cd session on my work laptop"*
resolves because the roster's `device_name` column matches it, and a
sanitized hostname could never serve that purpose as well. The prefix itself
you mostly won't see: it's stripped from every alias on the machine that
minted it (`muster agents` on work-laptop just shows `dotfiles/main`) and
shown in full everywhere else (`work-laptop-dotfiles/main` on your other
machine) — see the "Aliases are device-relative on screen" note in the
[README](../README.md#addressing) for the full display split, including why
an *agent* still reports itself by the full form even on its own machine.

The name lives in `<MUSTER_HOME>/device-name`, not an environment variable, so
it survives reboots without touching a shell profile and every process agrees
on it however it was launched. `$MUSTER_DEVICE_NAME` overrides it for one
shell.

**Renaming a machine — auto-adopted or explicit — is not retroactive, and
plain re-registration will not fix it.** A seeded alias is a *different*
alias, so re-registering under the new name creates a second identity and
leaves your inbox and read-state on the first. Nothing is lost, and mail to
the old alias still arrives — you just end up with two names for one
session. To carry the identity across properly, use the command built for
exactly this:

```sh
muster become work-laptop-ci-cd
```

That tombstones the old row, marks it superseded, and moves the identity and
its mail to the new alias.

Three more things worth knowing if you're renaming or upgrading an existing
install rather than starting fresh:

- **A running daemon doesn't pick up a rename until it restarts.** `muster
  device <name>` takes effect immediately for anything a CLI command mints
  (`register`, `become`, …), since each invocation reads the name file fresh.
  But the daemon reads its device name once at startup and holds it for the
  life of the process, so until you restart it, it keeps *expanding* a
  short, operator-typed name (for `send`, `nudge`, an MCP tool's `to_target`)
  against the *old* name even while new registrations are already seeded
  under the new one.
- **A bare `muster deregister` (no argument) can no longer find a row that
  predates this feature.** With no argument it reconstructs the alias
  `register` would mint for the current session — which is now always the
  seeded form — so it won't match an old, unprefixed row.
- **An old unprefixed row is visible but no longer addressable once the same
  session has re-registered under its seeded name.** You will see both in
  `muster agents` and in station, each shown in full so they don't read as
  one agent. But the old row's *only* address is its bare name, and a bare
  name on this machine now means the seeded one — so `muster inbox dotfiles`,
  `nudge`, `send`, `deregister`, a claim or an inbox read from an MCP tool,
  and even `muster become --from dotfiles` all reach the new row instead.
  (`become` is the one to watch: it retires the *new* row and its
  confirmation, printed short, reads as though it retired the old one.) What
  still works is anything that isn't an alias lookup: the row renders, `muster
  thread <id>` reads its threads by ID, and mail already addressed to it still
  lights its badge. Nothing carries the old row's leftover mail forward —
  read what you need by thread ID, then let `muster gc --purge-agents` reap
  the row once it's departed. There is deliberately no flag to address it
  directly: that would be a permanent escape hatch for a migration artifact.

### Getting the URL and token onto the other machine

Run this on the machine you deployed from:

```sh
muster-deploy -join
```

It prints the endpoint, the exact commands to run on the machine you are
adding, the token, and a fingerprint for checking the copy landed. The
commands and the token are printed **separately** on purpose: the paste-able
block uses `read -rs`, so the secret goes in at a blank prompt and never
enters the new machine's shell history.

**The token has to be moved by a human, and that is not an oversight.** It
cannot be fetched from AWS — it is a `NoEcho` stack parameter, so
CloudFormation will not return it, and more fundamentally the premise of this
backend is that a device needs no AWS credentials. A device with no
credentials cannot fetch its own credential. Something has to carry it across,
and that something is you.

Any channel you already trust works: a password manager is the best of them,
AirDrop is fine between two Macs in a room, `scp` if the machines can reach
each other, and typing it is entirely reasonable — it is 44 characters of
base64. What matters is not the channel but that you verify afterwards:

```sh
tr -d '\n' < ~/.local/share/muster/remote-token | shasum -a 256 | cut -c1-16
```

Compare that to the fingerprint `-join` printed. It is a hash, not the secret,
so it is safe to read aloud or paste anywhere — and `tr -d '\n'` means a
trailing newline from `echo` does not produce a false mismatch.

One thing to avoid: do not `cat` the token in a terminal a coding agent is
sharing. muster runs alongside agents that read their own terminals, and a
printed secret can land in a transcript that outlives the session.

**0. Check the binary is new enough.** Remote mode arrived in **v0.10.0**.

```sh
muster --version
```

This is the one prerequisite worth checking before anything else, because
getting it wrong fails silently in the worst possible way. On v0.9.1 and
earlier `MUSTER_BACKEND` and `MUSTER_REMOTE_URL` are not variables muster
knows about — they are simply ignored, no warning, no error — so the device
comes up on its own local SQLite bus while you believe it joined the hosted
one. Every symptom that follows (an empty roster, agents on other machines
that never appear) looks like a configuration problem, and every configuration
check you run will pass. Upgrade first.

**1. Install the token.** The token is deliberately not an environment
variable. muster runs alongside coding agents that read their own environment
as a matter of course, so a token in the environment is one `env` call away
from landing in an agent's context or a session transcript. It lives in a file
instead, and the daemon refuses to read that file unless it is mode 0600 —
a warning on a daemon's stderr is a warning nobody sees, so a loose mode is a
hard error.

```sh
mkdir -p ~/.local/share/muster
printf '%s' '<the token>' > ~/.local/share/muster/remote-token
chmod 600 ~/.local/share/muster/remote-token
```

(If you have set `MUSTER_HOME`, the file goes in there instead.)

**2. Put the backend selection in your shell profile**, not just in the shell
you happen to be sitting in:

```sh
export MUSTER_BACKEND=remote
export MUSTER_REMOTE_URL=https://xxxxxxxxxx.execute-api.us-east-1.amazonaws.com
```

This matters more than it looks. Any muster command will start the daemon for
you if the socket is dead, and the daemon it starts inherits the environment of
whatever spawned it. If these variables are missing from the environment a
stray `muster agents` runs in, that command silently brings up a *local*
daemon, and the device ends up on a private SQLite bus while every other
machine talks to the hosted one. Nothing errors; the roster is just empty.

**3. Restart any daemon that is already running.** A daemon reads the backend
selection once at startup, so an already-running local-mode daemon will not
switch.

Editing your profile in step 2 did not change the shell you edited it in, and
that shell is the one about to respawn the daemon — so pick up the new
environment first. Otherwise you walk straight into the failure the previous
paragraph describes: `muster agents` inherits a profile-free environment and
brings up a *local* daemon, silently.

```sh
exec $SHELL -l                      # or: open a fresh terminal
env | grep MUSTER_                  # both variables must be here before you continue
pkill -f 'muster serve'
muster agents                       # respawns the daemon, now in remote mode
```

`muster serve` logs one line to stderr when it starts. In remote mode, a
missing URL, a missing or badly-permissioned token, and an unresolvable device
identity are all resolved *before* the socket is bound, so a misconfigured
device fails immediately with the reason rather than on whichever command you
happen to run first.

**4. Check it.** Run `muster agents` on two devices. Once agents have
registered on both, each should see the other's.

## Configuration reference

On each device:

| Variable | Default | Meaning |
|---|---|---|
| `MUSTER_BACKEND` | `local` | `local` or `remote`. An unrecognised value is an error, not a fallback to local. |
| `MUSTER_REMOTE_URL` | — | The API endpoint from the stack's `MusterUrl` output. Required when `MUSTER_BACKEND=remote`. |
| `MUSTER_POLL_INTERVAL` | `10s` | Base cadence for the cross-device wake poll (a Go duration). Unparseable or non-positive values warn and fall back to the default. |
| `MUSTER_DEVICE_ID` | a persisted UUID | Overrides this device's identity. Normally left alone. |
| `MUSTER_HOME` | `~/.local/share/muster` | Data directory. Holds `remote-token` and `device-id`. |

The bearer token has no environment variable by design; see above.

On the Lambda function, all set for you by the CloudFormation template:

| Variable | Set from | Meaning |
|---|---|---|
| `MUSTER_DDB_TABLE` | the stack's table | Table the function opens. Required. |
| `MUSTER_TOKEN` | `MusterToken` parameter | The accepted bearer token. If unset, every request is rejected with 401 — the function fails closed. |
| `MUSTER_TOKEN_PREVIOUS` | `MusterTokenPrevious` parameter | A second accepted token, during a rotation only. Omitted from the function's environment when the parameter is empty. |
| `MUSTER_DDB_EVENT_RETENTION` | `EventRetention` parameter | Go duration after which DynamoDB TTL reaps a journal event. Default `720h`. A value the store cannot parse fails the cold start rather than silently reverting. |

## What to expect

**Latency.** A warm call is roughly 30–50ms: the network round trip plus a
DynamoDB query. Expect one cold start of 200–400ms after an idle gap — the
first command of the working day, typically — plus the occasional blip when
Lambda recycles an execution environment. It is not a per-call tax; the poll
traffic keeps an environment warm for as long as a device has live agents.

**Cross-device wake is a poll, not a push.** A write from this device
reconciles its own tmux badges inline and never waits for a tick. A message
from *another* device is noticed by the poller, which runs every ten seconds by
default, widens to at most one minute while the bus is quiet, and snaps back to
ten seconds the moment a tick finds mail. There is also no reconcile at
startup, so the first check happens one interval after `muster serve` — a badge
can be that stale immediately after a restart.

**`muster gc` behaves differently.** Its dead-agent reaping works as it always
did. Its journal-pruning half is a no-op on this backend, because DynamoDB's
native TTL does that job instead, driven by `MUSTER_DDB_EVENT_RETENTION`.

**The device binary is slightly larger.** About 13.7MB rather than 11.7MB,
because remote mode links `net/http` and `crypto/tls`. Those are the release
artifact's own numbers — darwin/arm64, built the way the release workflow
builds it, with `-trimpath -ldflags "-s -w"`. Build it yourself without those
flags and you will see roughly 20MB instead; that is the debug information, not
something anyone downloads. The AWS SDK is *not* in either one — it is
compiled only into the Lambda artifact, behind a build tag.

## Rotating the token

The function accepts two tokens so a rotation can roll across devices instead
of breaking all of them at once. Do it in four steps and do not skip the last
one.

**1. Move the current token to the previous slot and deploy the new one.**

```sh
NEW=$(openssl rand -base64 32)
aws cloudformation deploy \
  --template-file contrib/cloudformation/muster-backend.yaml \
  --stack-name muster --capabilities CAPABILITY_IAM --region us-east-1 \
  --parameter-overrides \
      MusterToken="$NEW" \
      MusterTokenPrevious="<the current token>" \
      CodeS3Bucket=my-muster-artifacts \
      CodeS3Key=muster-lambda-arm64-<tag>.zip
```

Both tokens now work. Nothing has broken.

**2. Roll each device** — write `$NEW` to `~/.local/share/muster/remote-token`
with mode 0600 and restart the daemon, exactly as in the device setup above.

**3. Confirm every device is on the new token** before proceeding. A device you
forget will keep working right up until the next step and then stop.

**4. Retire the old token** by deploying again with `MusterTokenPrevious=""`.
The parameter is not optional housekeeping: until you clear it, the leaked or
retired credential you were rotating away from is still live.

## Upgrading the function

Devices self-update with `muster update`; the Lambda function upgrade below stays the manual S3/CloudFormation path.

Upload the new zip under a new, version-stamped key and point the stack at it:

```sh
aws s3 cp muster-lambda-arm64-<new-tag>.zip s3://my-muster-artifacts/
aws cloudformation deploy \
  --template-file contrib/cloudformation/muster-backend.yaml \
  --stack-name muster --capabilities CAPABILITY_IAM --region us-east-1 \
  --parameter-overrides \
      MusterToken="<current token>" \
      CodeS3Bucket=my-muster-artifacts \
      CodeS3Key=muster-lambda-arm64-<new-tag>.zip
```

Reusing one key does not work: CloudFormation compares the key, not the bytes
behind it, and will report no changes while the old binary keeps serving. If
your bucket has versioning on, passing `CodeS3ObjectVersion` is the other way
to make a same-key upgrade take effect.

Device binaries and the function do not have to be upgraded in lockstep for
routine releases, but upgrade the function first when a release changes the
wire protocol.

## Known limitations

These are all known and accepted for this version. Each one is here because
running into it without warning would reasonably look like a bug.

**Listing the blackboard scans the base table.** `kv_list` performs one complete
DynamoDB scan, keeps only KV items (and the optional literal key prefix), then
sorts them in memory. The blackboard is intentionally small, so this avoids a
new index and pagination contract. Do not use it as a large object store.

**A notification can be missed — on this backend only.** When two writers
commit into the same recipient's mailbox at the same moment, one entry's
*notification* can be buried: the unread count and the tmux badge for it are
lost. The message itself is not. It is still listed by `muster inbox` and still
returned by `get_thread`. What you lose is the nudge, not the mail. This comes
from entry ids being allocated before the write commits, so two concurrent
writers can commit out of id order; the window is bounded to writers landing in
the same recipient's partition. The SQLite backend does not have this window at
all, because it serializes on a single connection. The full analysis is in the
package comment of `internal/dynamostore/store.go`.

**A live journal feed can drop a line — on this backend only.** `muster events`
in follow mode, and station's live feed, tail the journal by remembering the
last id they saw and asking for everything after it. That query reads a
DynamoDB global secondary index, which is eventually consistent and cannot be
read strongly consistent at any price. If two events are written in order but
replicate into the index out of order, a poll can return the later one, advance
its watermark past the earlier one, and never see it again. The line is not
delayed; it is gone from that feed. Nothing else is affected: the event is in
the table, and a *backlog* read — `muster events` without `--follow`, which is
ordered newest-first rather than bounded by a watermark — still returns it. So
if you need a complete journal, read the backlog rather than trusting a feed
you left running. The journal is observability only; no wake, badge, message,
task or read-state depends on it. The SQLite backend does not have this window,
because there ids are allocated by AUTOINCREMENT inside the inserting
transaction on a single pinned connection, so a follow read cannot see a gap it
will then skip past. The full analysis is above `Events` in
`internal/dynamostore/events.go`.

**There is no migration path.** A remote bus starts empty. There is no export
from an existing local SQLite bus and no import into the hosted one. Your
existing threads, tasks, and read state stay on whichever machine they are on.

**Wake is a poll, so badges lag.** Ten seconds by default, up to a minute while
the bus is quiet, and there is no reconcile at daemon startup — so a badge can
be one full interval stale right after `muster serve`. Local writes are not
affected; they reconcile inline.

**Offline means no bus.** There is no queueing of writes while the network is
down. This matches the local behaviour where no daemon means no bus, but on a
laptop it comes up more often.

**Deleting the stack deletes the bus.** The table has no backup, no
point-in-time recovery, and no export. `aws cloudformation delete-stack` takes
every message with it. (The template does set `UpdateReplacePolicy: Retain`, so
a CloudFormation *replacement* — the kind triggered by editing an immutable
property — orphans the old table rather than discarding it. That protects
against an accident, not against a deliberate delete.)

## Troubleshooting

**The stack fails to create, naming `UnreservedConcurrentExecution`.** The
error reads roughly:

```
CREATE_FAILED  MusterFunction  Specified ReservedConcurrentExecutions for
function decreases account's UnreservedConcurrentExecution below its minimum
value of [100]
```

This is not about your parameters being wrong. Lambda refuses any reservation
that would leave the account with less than 100 unreserved concurrency, and a
new AWS account's regional concurrency quota can be as low as 10 — below the
floor before you reserve anything. On such an account *every* positive
`ReservedConcurrency` fails, so there is no value you can retry your way into.

Deploy with the reservation off:

```sh
aws cloudformation deploy ... --parameter-overrides ReservedConcurrency=0 ...
```

`0` omits the property rather than setting a limit of zero, so the function
runs on the account's shared pool. Then request a Lambda concurrency quota
increase (Service Quotas → Lambda → "Concurrent executions"; the usual grant is
1000) and redeploy with `ReservedConcurrency=10` to get the cost guard back.
Until you do, nothing bounds what a runaway poller or a stranger who found your
URL can spend — so set an AWS Budgets alert, which you should have anyway.

**Every request returns 500, and the function's logs are empty.** The function
was never invoked, so the failure is between API Gateway and Lambda rather than
in your token or your code. The usual cause is the missing invoke permission:
`MusterApiPermission` is what lets `apigateway.amazonaws.com` call the
function, and without it the integration fails before the handler runs. If you
have hand-edited the template, check that resource is still there and that its
`SourceArn` names this API. The API access log group (the stack's
`ApiAccessLogGroupName` output) records an `integrationErrorMessage` for these,
which is the fastest confirmation.

**Every request returns 429.** Route throttling. The default is 20 requests per
second across all devices, which a normal fleet does not approach — so either
something is polling far harder than you think, or someone is hammering the
endpoint. Look at the access log before raising `ThrottleRateLimit`; the limit
is doing its job in the second case.

**Every request returns 403 with a message about Function URL authorization.**
This is a stale endpoint, not a broken deployment. Earlier versions of this
backend used a Lambda Function URL with `AuthType: NONE`, and some AWS
Organizations deny anonymous Function URL invocation by service control policy
— which surfaces exactly this way: 403 at the edge, nothing in the function's
logs, and no indication anywhere that a guardrail is the cause. Confirm the
function itself is healthy with a direct `aws lambda invoke`; if that returns
`{"ok":true,...}` while the URL does not, you are hitting the guardrail. The
current template does not create a Function URL at all, so the fix is to
redeploy and take `MUSTER_REMOTE_URL` from the `MusterUrl` output. If an old
Function URL is still attached to the function from a previous deploy, delete
it — it is an unauthenticated endpoint nobody is watching.

**Every request returns 401.** The token the device sent did not match. Check
that `remote-token` on the device holds exactly the token you deployed —
surrounding whitespace is not the problem, since the device trims it, so a
trailing newline from `echo` is harmless — and that `MUSTER_TOKEN` is actually
set on the function. If it is unset the function fails closed and rejects
everything, and it says so once at cold start in CloudWatch.

**The function fails at every cold start with an error about TTL.** Read the
error carefully: this is usually an IAM problem wearing a TTL costume. The
store verifies TTL on every open, and `dynamodb:DescribeTimeToLive` is a
*distinct* IAM action from `dynamodb:DescribeTable` — granting the second does
not grant the first. The bundled template grants both. A hand-rolled role that
only covers the data plane will fail here, at every single cold start, with a
message that sends you off investigating DynamoDB TTL instead of your policy.

**The function exits at cold start saying it was "built without lambda mode".**
The zip contains a binary built without `-tags lambda`, so lambda mode compiled
out to a stub. Use the release's `muster-lambda-arm64-*.zip` from muster.tools/dl rather than
a locally cross-compiled binary, or build it the way the release workflow does.

**`muster agents` shows an empty roster on one device.** Almost always that
device is running a local daemon, and there are two ways to get one. First run
`muster --version`: on anything older than **v0.10.0** there is no remote mode
at all, both variables are ignored without complaint, and no amount of checking
your exports will reveal it — the exports are fine, the binary is not. Upgrade
and restart. If the version is new enough, the daemon was spawned from an
environment missing the variables: check `env | grep MUSTER_` in the shell you
are actually in, `pkill -f 'muster serve'`, and try again from a shell that has
them.

**The daemon refuses to start, naming the token file's mode.** `chmod 600` it.
This is deliberate: a token file that others can read is not meaningfully safer
than the environment variable it exists to avoid.

**Diagnostics.** There are two log groups and the distinction matters. The
function writes everything to stderr, which lands in the group named by the
stack's `LogGroupName` output — `aws logs tail /aws/lambda/muster-bus --follow`
is usually the fastest way in. The API writes an access-log line per request to
the group named by `ApiAccessLogGroupName`, and that one is the only place a
request rejected *before* the function ran shows up at all. When the Lambda log
group is empty but requests are failing, the access log is where to look.

## Going back to local

Unset `MUSTER_BACKEND` and `MUSTER_REMOTE_URL`, restart the daemon, and the
device is back on its own SQLite bus with its old state intact — the local
database was never touched. Delete the stack when no device needs it any more,
remembering that this destroys the shared bus permanently.
