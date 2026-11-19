# Pushing a flow file to a tenant's stations

A tenant in EmberNet can run several HotLoop Flow installs, one at each plant or line. We call each one a station. This page is about putting one flow file, and so one set of FlowLab diagrams, on all of them, checking each one, and putting a station back if it fails.

The short version: `hotloop-flow stations push -tenant acme -file flows.json` pushes to the stations EmberNet shows as running for `acme`, one at a time. If a station is not right afterwards, that station goes back to what it had and the push stops. If the file would restart a running logger, nothing happens until you say that may.

## Where the stations come from

Nobody types a station in. The list is what the EmberNet dashboard shows as running for the tenant, the same list its Running Apps page shows: every app labeled `hotloop-flow` that is running and belongs to the tenant. A station is reached at its Flux address when the dashboard reports one, and at its cluster address otherwise, so run the push from somewhere that address resolves.

The tenant is always named, and a push is only ever for that one tenant. The dashboard is asked with your dashboard token and holds you to the tenants you may see, and we check the answer again: a row that arrives for any other tenant is dropped and counted, never used. A tenant with no running Flow answers an error.

## The command

```bash
hotloop-flow stations list -tenant acme
hotloop-flow stations push -tenant acme -file flows.json -note "new line 3 logger"
```

The dashboard address comes from `-dashboard` or `HOTLOOP_FLOW_DASHBOARD_URL`, and its token from `-dashboard-token-file` or `HOTLOOP_FLOW_DASHBOARD_TOKEN`. The token the stations accept comes from `-token-file` or `HOTLOOP_FLOW_TOKEN`, as for `hotloop-flow deploy`. Make it with `hotloop-flow token`, and give each station that token as an API token with `flows.read`, `flows.write` and `status.read` (a plain deploy token has only the first two): the check needs to read each station's diagrams, and a plain deploy token cannot. The stations of one tenant take the same token.

| Flag | What it does |
|---|---|
| `-dry-run` | Plan every station, and change none. Shows what is live, what each would restart, and which ones could not be planned. |
| `-confirm-restart` | You agree that running loggers may be restarted by this push. |
| `-window 02:00-04:00` | The hours a restart is agreed in advance, in `-window-zone` (an IANA name, UTC when left out). A push inside the window goes ahead. It may run past midnight, `22:00-02:00`. |
| `-settle 5s` | How long a station's diagrams get to start before the check calls them not running. |
| `-json` | The report as JSON. |

Exit status 0 is every station done. 3 is a station failed and the push stopped. 4 is the push waiting for you to agree to restarts. 5 is stations skipped because they did not answer or could not be planned.

## What a push does

First it plans every station without changing one. For each it reads the live flow file and revision, finds the deployment record it could go back to, lists its diagrams, and works out which running loggers the file would restart. Then, if nothing needs a yes, it goes station by station in name order.

1. **Deploy** the file with the revision the plan saw, so a deploy somebody made in between is refused instead of overwritten. Only what changed restarts. Credentials never travel: each station keeps its own passwords, and an instrument's writes stay off until someone on that station turns them on.
2. **Check.** The station must answer the revision every station is to have. The revision comes from the file's content, so the same file is the same revision everywhere, and the push says what it is before it starts. Every diagram in the file that starts by itself must be on the station, with no problem, and running or finished within the settle time. A node that did not start fails the check too.
3. **A station that fails the check goes back.** It is rolled back to the deployment it was on, as a deployment of its own, and checked to be on its old revision. The push stops there.

The report says what happened to each station:

| Status | Meaning |
|---|---|
| `done` | Deployed and past the check. |
| `unchanged` | Already on this revision, and its diagrams are as they should be. |
| `failed` | Not deployed (the station refused it, or somebody deployed in between), or already on this revision and not right. Nothing was changed. |
| `reverted` | Failed the check and is back on the revision it had. |
| `revert-failed` | Failed the check and could not be put back. This one needs a person. |
| `skipped` | Not touched. It did not answer, or it holds no deployment record of its live revision, so a failed push could not be undone there. Deploy to it once by hand, or fix the link, and push again. |
| `not-started` | The push stopped before this station. It is as it was. |
| `planned` | A dry run's answer. |

Stations that were skipped do not stop the push. A push is repeatable: a station already on the revision is `unchanged`, so a second push finishes only what the first did not.

## Restarts

A diagram that starts by itself is a logger. Deploying a changed one restarts it, and a removed one stops. A push that would do that to a logger that is running stops before it touches anything, and the report names each station and each logger. Then one of three things lets it go ahead:

- you answer yes at the prompt, when the command is run at a terminal;
- you pass `-confirm-restart`;
- the time is inside the window you gave with `-window`.

Nothing restarts without one of them. A diagram that did not change keeps its run, and a new one just starts.

## From an instance, and for an agent

An instance can push to its own tenant's stations over its admin API, which is what the MCP tools `list_stations` and `push_to_stations` use. Set all four of these, in the config file under `stations:` or by environment, and the routes `GET /stations` and `POST /stations/push` appear. Without them they do not exist.

| Setting | Environment | What it is |
|---|---|---|
| `stations.dashboardUrl` | `HOTLOOP_FLOW_STATIONS_DASHBOARD_URL` | The EmberNet dashboard that knows the tenant's installs. |
| `stations.tenant` | `HOTLOOP_FLOW_STATIONS_TENANT` | The one tenant this instance pushes for. A request cannot name another. |
| `stations.dashboardTokenFile` | `HOTLOOP_FLOW_STATIONS_DASHBOARD_TOKEN_FILE` | A file holding the dashboard token. |
| `stations.deployTokenFile` | `HOTLOOP_FLOW_STATIONS_DEPLOY_TOKEN_FILE` | A file holding the deploy token the stations accept. |

In the chart these are `stations.dashboardUrl`, `stations.tenant`, `stations.dashboardTokenSecret` and `stations.deployTokenSecret`, each Secret holding one token and mounted as a file; with Podman and Quadlet they are the four environment variables and two mounted secrets in [deploying.md](deploying.md). Set all four or none: a chart render with some is refused, and so is a start with some.

`POST /stations/push` needs `flows.write` and takes `{flows, note, confirmRestart, window, windowZone, dryRun}`. Leave `flows` out and it pushes what this instance runs. It answers with the same report, plus `ok`, and a push is in the audit log as `stations.push`. `GET /stations` needs `flows.read`. A push that is waiting for a yes answers with `needsConfirm` true and has changed nothing: send it again with `confirmRestart` or a window.

## What is not proven

This was run against three real Flow instances in tests and on The Beast, with the dashboard's answer served by a stand-in, because the dashboard needs a signed-in person and a cluster. The shape of the dashboard's list was read from the live dashboard for a real tenant. It has not been pushed to a tenant's stations through Flux, so whether the address the dashboard gives is reachable from where you run the push is yours to check with `stations list` and a `-dry-run`. The chart values and the Quadlet lines below were rendered and the chart check passes in CI, but they were not run on a cluster or a box.
