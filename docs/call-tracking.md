# Call tracking and failover

**Call tracking, recovery on start and failover work on any cluster today.** With call tracking on, each FreeSWITCH
node records every live call leg in a PostgreSQL that the whole fleet shares. With `spec.recovery`, a FreeSWITCH that
starts takes back the calls tracked under its address: after a crash, or in a pod the operator's
[failover](freeswitch-fleet.md#health-and-failover) started on a spare with the failed member's address. This page
covers setting tracking and recovery up, and how failover uses them.

## What it takes

- **An image with `mod_pgsql`.** The project's own, `ghcr.io/spiegela/freeswitch`
  ([`images/freeswitch`](../images/freeswitch/Dockerfile)), has it. Pin it by
  digest in `spec.image`, or build it yourself:

  ```sh
  docker build -t <registry>/freeswitch:<tag> images/freeswitch
  ```

- **A PostgreSQL that every edge node can reach.** It holds only live state: rows go when calls end. Each node keeps
  two connections open.
  - **kind and small clusters:** a small instance in the cluster. [`deploy/kind/postgres`](../deploy/kind/postgres/postgres.yaml)
    is a starting point. On a real cluster, set a storage class and size, and take the Secret from your secret store
    instead of the committed kind value.
  - **A managed service,** such as Aurora PostgreSQL, through its writer (cluster) endpoint.
- **[`deploy/postgres/freeswitch-core-db.sql`](../deploy/postgres/freeswitch-core-db.sql) run once in that
  database,** below.
- **Five settings in the FreeSWITCH configuration**, below.

## Prepare the database

**Run [`freeswitch-core-db.sql`](../deploy/postgres/freeswitch-core-db.sql) once, before or after FreeSWITCH first
starts**, as a user allowed to create event triggers (a superuser; on Aurora or RDS, the master user):

```sh
psql "host=<writer endpoint> dbname=freeswitch user=<master user> sslmode=require" -v ON_ERROR_STOP=1 \
  -f deploy/postgres/freeswitch-core-db.sql
```

FreeSWITCH creates its tables with length limits (`varchar(1024)` for a caller's name) that SQLite never enforces
and PostgreSQL does. One call over a limit loses other calls' writes: FreeSWITCH writes the core db in batches of
one transaction each, a refused statement ends its batch, and the batch's COMMIT rolls back everything before it,
never retried. That takes the call tracking failover needs, and the deletes of ended calls, leaving `channels` rows
for calls long gone. The script turns every length-limited column into `text`, now and, through an event trigger,
whenever FreeSWITCH creates or alters a table later (a first start, an upgrade). FreeSWITCH keeps the tables it
finds, so the change lasts. Use a database FreeSWITCH alone uses: the script changes every table in it.

Without the right to create event triggers, leave the trigger out and run `SELECT freeswitch_unbound_columns()`
after FreeSWITCH first starts and after each upgrade. `hack/kind.sh deploy` runs the script on kind, and
`hack/kind.sh bench` fails if PostgreSQL refused any statement during the run (sipbench's `raw-large-messages`
sends a caller name over the limit).

## Configure FreeSWITCH

sipbench's configuration ([`sipbench/freeswitch/conf`](../sipbench/freeswitch/conf/)) already has all five. Each one is
read from the environment, so the same ConfigMaps serve compose (tracking off) and a cluster (tracking on).

1. **Load `mod_pgsql` before the core db opens.** Use [`pre_load_modules.conf.xml`](../sipbench/freeswitch/conf/autoload_configs/pre_load_modules.conf.xml),
   not `modules.conf.xml`, which loads too late:

   ```xml
   <configuration name="pre_load_modules.conf">
     <modules><load module="mod_pgsql"/></modules>
   </configuration>
   ```

2. **Point the core db at PostgreSQL** in [`switch.conf.xml`](../sipbench/freeswitch/conf/autoload_configs/switch.conf.xml).
   An empty value leaves it in SQLite:

   ```xml
   <param name="core-db-dsn" value="$${core_db_dsn}"/>
   ```

3. **Turn on `track-calls` on every SIP profile a call crosses.** Here that's `trunks` and `backend`. A bridged call
   can only be recovered with both legs, so tracking one profile isn't enough:

   ```xml
   <param name="track-calls" value="$${track_calls}"/>
   ```

4. **Read both values from the environment** in [`vars.xml`](../sipbench/freeswitch/conf/vars.xml)
   ([`bin/env.sh`](../sipbench/freeswitch/conf/bin/env.sh) prints a variable, or a default when it's unset):

   ```xml
   <X-PRE-PROCESS cmd="exec-set" data="core_db_dsn=/bin/sh /usr/local/freeswitch/conf/bin/env.sh CORE_DB_DSN"/>
   <X-PRE-PROCESS cmd="exec-set" data="track_calls=/bin/sh /usr/local/freeswitch/conf/bin/env.sh TRACK_CALLS false"/>
   ```

5. **Name each SIP profile for the node's address, and keep its plain name as an alias.** Each tracked leg's row
   names its profile, and `sofia recover` takes over every row another FreeSWITCH wrote under the name of a profile
   it runs, whichever node wrote it. With the same `trunks` and `backend` on every node, recovering on one node takes
   every other node's calls too, and breaks them: on kind, a restarted node recovered six legs (its own call's two,
   and four from two live nodes, whose calls then dropped). Named for the address, a node's profiles match only rows
   written under that address: its own from before a restart, or a lost node's once failover moves the address to
   it. [`bin/profile-name.sh`](../sipbench/freeswitch/conf/bin/profile-name.sh) prints the name from the pod's
   external address (`trunks-203-0-113-10`), or the plain name where the pod has none, and
   [`vars.xml`](../sipbench/freeswitch/conf/vars.xml) reads it at start:

   ```xml
   <X-PRE-PROCESS cmd="exec-set" data="trunks_profile=/bin/sh /usr/local/freeswitch/conf/bin/profile-name.sh trunks"/>
   ```

   ```xml
   <profile name="$${trunks_profile}">
     <aliases>
       <alias name="trunks"/>
     </aliases>
   ```

   The alias keeps dial strings (`sofia/trunks/…`) and commands (`sofia status profile trunks`) as they were. The
   exporter reports a profile named `<alias>-<anything>` under its alias, so the `profile` label on the
   `freeswitch_sofia_*` series stays `trunks` and `backend` ([metrics](metrics.md)). Both profiles need it: a call
   is recovered only with both legs. It needs an address per node, known when FreeSWITCH starts, since the name is
   fixed then: a fleet with `externalAddress.source: None` (every node plain `trunks`) still recovers everyone's
   calls, so don't run `sofia recover` there (`spec.recovery` refuses such a fleet). The address must be `required`
   (the default), on a pool or from the
   node (`NodeExternalIP`): the operator then holds FreeSWITCH back until it has the address (the `wait-for-address`
   init container), and FreeSWITCH never names its profiles plainly. With `required: false` a pod can start before
   its address and keep plain names until it restarts.

   A copy of this configuration (platform-gitops' voice edge keeps one) must ship `bin/profile-name.sh` with the rest
   of `conf/bin`: without it the profile names come out empty and FreeSWITCH never turns ready.

Only the core db moves. Each SIP profile's own tables, such as registrations it holds, stay in the pod's SQLite.
Recovery doesn't need them.

## Configure the `FreeswitchCluster`

Set the image and three variables in `spec.env`, as [`deploy/kind/freeswitchcluster.yaml`](../deploy/kind/freeswitchcluster.yaml)
does:

```yaml
spec:
  image: ghcr.io/spiegela/freeswitch@sha256:…  # images/freeswitch
  env:
    - name: CORE_DB_DSN                         # libpq keywords after pgsql://
      value: "pgsql://host=postgres.telephony.svc.cluster.local dbname=freeswitch user=freeswitch connect_timeout=5"
    - name: PGPASSWORD                          # libpq reads it; keep it out of the DSN
      valueFrom: {secretKeyRef: {name: postgres, key: password}}
    - name: TRACK_CALLS
      value: "true"
```

For Aurora, use the writer endpoint as `host` and add `sslmode=require`.

**Turn on recovery on start** with `spec.recovery: {}`:

```yaml
spec:
  recovery: {}
```

Each time FreeSWITCH starts in a pod (a new pod, or the kubelet restarting a crashed one), the operator's postStart
hook waits for every SIP profile in FreeSWITCH's `sofia.conf` to run, then runs `sofia recover`, which takes over
every call leg tracked under the name of a profile it runs and re-invites both far ends. The kubelet runs no probe
until the hook returns, so the pod turns ready, and Kamailio sends it new calls, only once the old ones are back.
The hook:

- **Recovers only under names made for the pod's address.** Every profile must be named `<name>-<address>` (step 5
  above). One that isn't would take other nodes' live calls, so the hook recovers nothing and says which profile
  stopped it. The API refuses `recovery` on a fleet whose address isn't required (`source: None`, or `required:
  false`).
- **Waits for every profile.** A bridged call is recovered only with both legs, and FreeSWITCH starts its profiles
  one by one. When one isn't running within 120 s (as long as the startup probe gives FreeSWITCH) the hook recovers
  nothing: the calls drop, as they would without it.
- **Never fails.** A failed postStart hook kills the container, and FreeSWITCH restarting wouldn't recover the calls
  either.
- **Says what it did** in the container's log (`freeswitch-operator: recovery: Recovered 4 call(s)`) and in
  `/run/freeswitch-operator/recovered`, on a volume the exporter shares: with `spec.metrics`, the
  `freeswitch_recovery_*` series report it ([metrics](metrics.md)), and `FreeswitchRecoverySkipped` fires when a
  start recovered nothing because something was wrong. sipbench reads the file too.

Turning it on, or off, changes the pod template, so it rolls the fleet one drained node at a time.

Things to know:

- **Keep the password out of the DSN.** FreeSWITCH logs the DSN when a connection fails. In `PGPASSWORD` it never
  reaches the logs.
- **FreeSWITCH reads the password only when it starts.** A rotated Secret reaches a node only when its pod restarts,
  so don't let a secret store rotate it on its own schedule. One way is to generate it once (an External Secrets
  password generator with `refreshPolicy: CreatedOnce`).
- **Bring the database up first.** With a DSN set, FreeSWITCH won't start until the database answers. A node that
  can't reach it never becomes ready, so a roll stops there.
- **Once running, a database restart is harmless.** FreeSWITCH logs the lost connection, keeps its calls and
  reconnects by itself. This was checked on kind by restarting PostgreSQL under a live edge. An Aurora failover
  should look the same to FreeSWITCH, but hasn't been checked.
- **The edge runs on the host network.** The operator sets `ClusterFirstWithHostNet`, so Service names resolve. But
  the database sees connections coming from the nodes' own addresses, not pod addresses. A NetworkPolicy or
  security group in front of it must allow the edge nodes' addresses.
- **Set a DSN only on an image that has `mod_pgsql`.** On another image the module fails to load, and FreeSWITCH
  only logs that. After changing images, check the edge's log for a `mod_pgsql` load error.

## Check it

While a call is up, each of its legs is a row in the `recovery` table under its node's name. The rows go when the
call ends:

```sh
kubectl -n telephony exec postgres-0 -- psql -U freeswitch -d freeswitch \
  -c 'select hostname, profile_name, uuid from recovery'
```

The core db's tables (`channels`, `calls`, `registrations`, `recovery`, ...) are listed by `\dt` in the same database.
None should have a length limit left:

```sh
kubectl -n telephony exec postgres-0 -- psql -U freeswitch -d freeswitch -c "select table_name, column_name \
  from information_schema.columns where table_schema = current_schema() and character_maximum_length is not null"
```

PostgreSQL's log should show no refused statements beyond the `create index` ones FreeSWITCH repeats at every start
(`relation ... already exists`, harmless: they run on their own, not in a batch).

## Failover: what tracking is for, and what's left

When a node is lost, the plan is:

1. **Fence it.** Its address is unbound before anything else happens, and can never come back to it. The operator
   labels and taints the node fenced, and a pool takes its addresses back only where the pool may fence: its fencing is
   `Authoritative`, or it sets `allowAdvisoryFencing` ([address pools](address-pools.md)).
2. **Move its addresses to a spare.** The operator deletes the pod, which comes back on the spare.
3. **Recover its calls on the spare.** FreeSWITCH rebuilds each tracked leg from its row. It sends each far end a
   re-INVITE built from the leg's saved dialog (Contact, Via, route), from the moved address, and bridges the legs
   again.

**Recovery takes one address's calls.** As FreeSWITCH has it (1.10.10 through 1.11.4), `sofia recover` takes over
every row another FreeSWITCH wrote (`runtime_uuid != own`) under the name of a profile it runs, and deletes them. It
never looks at which node wrote a row. With each profile named for its node's address (step 5 above), that is
exactly the calls of the address the recovering node holds. A node whose FreeSWITCH restarts in place (a crash, an
OOM kill) keeps its address, so `sofia recover` there takes back its own calls and nothing else. With
`spec.recovery`, that happens as FreeSWITCH starts again. sipbench's `crash-recovers-own-calls` checks it on kind: it
crashes FreeSWITCH under a call, and checks that the recovery on start took exactly that node's legs and that every
call on every node is still up with audio both ways. A call recovered in place is re-invited from the same node and
address, so its in-dialog SIP needs nothing new. Recovery skips a row whose profile the recovering node doesn't run:
on failover, the spare must come up holding the lost address.

**In-dialog SIP follows a recovered call to its new node.** The re-INVITEs recovery sends carry the recovering
node's Contact (its own address), so both far ends send their next requests there: the carrier's through Kamailio,
the backend's directly (a far end that keeps sending to the Contact of the dialog's first INVITE loses its BYE:
sipbench's runner had to put the refreshed Contact back before hanging up). Recovery binds each leg's RTP to the
address the leg had, so both legs' RTP must be on an address that moves with the pod: sipbench binds the backend's to
the pool's bind address as well as the carriers', and a leg bound to the node's own address fails on the new node
("Bind Error", then `INCOMPATIBLE_DESTINATION`). The bind address moves with the pod on MetalLB (it is the pool's
address itself) and on an AWS pool with `privateAddress: Secondary` when the new node's interface is in the same
subnet: the pool asks for the same secondary private address again ([address pools](address-pools.md)). Across
subnets (another availability zone), or with `privateAddress: Primary` (no bind address: RTP is on the node's own),
a call recovered on another node loses its media. FreeSWITCH's own requests follow the route it saved, through
Kamailio. Where Kamailio has a bind address, that route names Kamailio's pod rather than its node
([Kamailio](kamailio.md#routing)), so it still reaches Kamailio when Kamailio was replaced elsewhere. A FreeSWITCH
on a node new to the fleet sends from that node's address, which Kamailio's `trustedPeers` check would drop until
its dispatcher list names the node (`reloadDelaySeconds` later): the operator admits the address over JSON-RPC as
soon as it writes the list. sipbench's
`moved-call-in-dialog` checks a move on kind: it cordons a call's node, deletes the pod, crashes FreeSWITCH under its
drain (a crash alone would only restart it in place, where it recovers its own calls), deletes the pod at once, and
checks the replacement, which comes up on the spare node with the moved pod's address, recovered the
calls on start. Then every
call's backend sends a REFER and every caller hangs up, and each of those requests must reach the node the call is
on now. A pod replaced onto a spare keeps its address only when the spare holds none of its own (the pool's rule);
one that kept an idle address from an earlier pod gives the moved pod that one, and its calls' rows stay put.

Not built yet:

- **Rows nobody recovers stay.** When a failed member's replacement doesn't come up with its address (a spare that
  holds an idle address of its own, or no spare at all), its rows stay in `recovery` until a FreeSWITCH holding that
  address starts with `spec.recovery`.

With failover and `spec.recovery`, a lost node's calls move: the operator fences the node and starts the member on
the spare, the pool gives it the member's address, and FreeSWITCH recovers the calls as it starts there, before its
pod turns ready. A call is silent from the node's loss until then: 70 s on kind (the node NotReady after about
45 s, Failed 15 s later, then the pod's start on the spare). New calls go elsewhere meanwhile, within
Kamailio's 5 s failover at first, then with no delay once the dispatcher's probes have taken the node out.
sipbench's `node-lost-mid-call` checks it: it takes a call's node off the network for two minutes (tc drop filters
on the node's own interface), checks the rest of the fleet keeps taking calls, waits for the operator to fail the
member over to the spare, and checks the call is back there with audio both ways.

