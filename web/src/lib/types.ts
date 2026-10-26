// Hand-written from api/openrpc.json (protocol 1.0.0). Keep in step with the
// spec: field names, optionality and nullability follow it exactly. Timestamps
// are RFC 3339 strings. Unknown extra fields in results must be ignored.

export type Role = "viewer" | "operator" | "admin";
export type SubscriptionID = string;
export type Timestamp = string;

// ---------------------------------------------------------------- envelope

export interface Gap {
  from: number;
  to: number;
}

/** Params of every `<ns>.update` notification. Exactly one of data and gap. */
export interface SubscriptionUpdate<T = unknown> {
  subscription: SubscriptionID;
  seq: number;
  data?: T;
  gap?: Gap;
}

// ------------------------------------------------------------------- agent

export interface ClientInfo {
  name?: string;
  version?: string;
}

export interface HelloParams {
  protocol: string;
  client?: ClientInfo;
  capabilities?: string[];
}

export interface HelloResult {
  protocol: string;
  agent: { name: "ki-agent"; version: string; commit: string };
  capabilities: string[];
}

export interface ControlPlaneStatus {
  state: "disabled" | "connecting" | "connected" | "backoff";
  source?: "config" | "enrollment";
  url?: string;
  connected_since?: Timestamp;
  last_error?: string;
  reconnects?: number;
  next_attempt?: Timestamp;
}

export interface AgentInfo {
  version: string;
  commit: string;
  hostname: string;
  os: string;
  arch: string;
  uptime_ms: number;
  go_version: string;
  control_plane: ControlPlaneStatus;
}

export interface Heartbeat {
  time: Timestamp;
  uptime_ms: number;
}

export interface PingResult {
  pong: true;
  time: Timestamp;
}

export interface RuntimeInfo {
  name: string;
  version: string;
  api_version: string;
  os: string;
  arch: string;
}

// -------------------------------------------------------------- containers

export interface ContainerPort {
  private_port: number;
  public_port?: number;
  type: string;
  ip?: string;
}

export interface Container {
  id: string;
  name: string;
  image: string;
  state: string;
  status: string;
  project?: string;
  service?: string;
  created: Timestamp;
  labels?: Record<string, string>;
  ports?: ContainerPort[];
}

export interface ContainerMount {
  type: string;
  source: string;
  destination: string;
  read_only: boolean;
}

export interface ContainerDetail extends Container {
  started_at: Timestamp;
  finished_at: Timestamp;
  exit_code: number;
  restart_count: number;
  health?: "starting" | "healthy" | "unhealthy";
  mounts?: ContainerMount[];
  networks?: string[];
}

export interface LogLine {
  stream: "stdout" | "stderr";
  time?: Timestamp;
  text: string;
}

export interface LogEnd {
  eof: true;
  error?: string;
}

export type LogUpdate = LogLine | LogEnd;

// ---------------------------------------------------------------- monitoring

export interface HostSample {
  time: Timestamp;
  cpu: { percent: number | null; cores: number; per_core?: number[] };
  load: { "1": number; "5": number; "15": number };
  memory: { total: number; used: number; available: number; percent: number };
  swap: { total: number; used: number; percent: number };
  disks: Array<{
    path: string;
    device: string;
    fstype: string;
    total: number;
    used: number;
    free: number;
    percent: number;
    inodes_percent: number;
  }>;
  disk_io: {
    read_bytes_per_s: number | null;
    write_bytes_per_s: number | null;
    read_ops_per_s: number | null;
    write_ops_per_s: number | null;
  };
  network: {
    rx_bytes_per_s: number | null;
    tx_bytes_per_s: number | null;
    interfaces: Array<{ name: string; rx_bytes_per_s: number | null; tx_bytes_per_s: number | null }>;
  };
  uptime_s: number;
}

export interface ContainerSample {
  time: Timestamp;
  id: string;
  name: string;
  project: string;
  service: string;
  component: string;
  cpu_percent: number | null;
  memory: { usage: number; limit: number; percent: number | null };
  network: { rx_bytes_per_s: number | null; tx_bytes_per_s: number | null };
  block_io: { read_bytes_per_s: number | null; write_bytes_per_s: number | null };
  pids: number;
}

export interface HostStatsResult {
  current: HostSample | null;
  history?: HostSample[];
}

export interface ContainerStatsResult {
  available: boolean;
  containers: Array<{ current: ContainerSample | null; history?: ContainerSample[] }>;
}

export interface HostUpdate {
  sample: HostSample;
}

export interface ContainerStatsUpdate {
  available: boolean;
  containers: ContainerSample[];
}

// ----------------------------------------------------------------- projects

export interface ProjectTrigger {
  branch?: string;
  tag_pattern?: string;
}

export interface ProjectAuth {
  method: "none" | "ssh_key" | "https_token";
  public_key?: string;
  token_user?: string;
}

export interface Exposure {
  service: string;
  port: number;
  domains: string[];
  path?: string;
  source: "labels" | "project";
}

export interface Project {
  slug: string;
  repo_url: string;
  trigger: ProjectTrigger;
  default_branch: string;
  compose_file: string;
  auth: ProjectAuth;
  webhook: { path: string };
  status: "pending_access" | "ready";
  status_detail: string;
  commit: string;
  created_at: Timestamp;
  updated_at: Timestamp;
  exposure?: Exposure[];
  exposure_warnings?: string[];
}

export interface PolicyViolation {
  service: string;
  rule: string;
  detail: string;
}

export interface PruneReport {
  containers: number;
  networks: number;
  volumes: number;
  kept_volumes: string[];
}

export interface EnvNames {
  names: string[];
}

export interface EnvVarInfo {
  name: string;
  updated_at: Timestamp;
}

export interface ProjectCreateParams {
  repo_url: string;
  slug?: string;
  branch?: string;
  tag_pattern?: string;
  compose_file?: string;
  token?: string;
  token_user?: string;
}

export interface ProjectUpdateParams {
  slug: string;
  branch?: string;
  tag_pattern?: string;
  compose_file?: string;
  token?: string;
  token_user?: string;
  rotate_webhook_secret?: boolean;
  rotate_deploy_key?: boolean;
}

// ----------------------------------------------------------------- services

export type ServiceState = "running" | "partial" | "stopped" | "unhealthy" | "missing";

export interface ServiceContainer {
  id: string;
  name: string;
  state: string;
  status: string;
  health: string;
  restart_count: number;
  started_at?: Timestamp;
}

/** Wire view of one service of a project (projects.services). */
export interface ProjectService {
  name: string;
  image?: string;
  exposure?: Exposure;
  containers: ServiceContainer[];
  state: ServiceState;
}

// ------------------------------------------------------------------- deploys

export type DeploySource = "webhook" | "api" | "cli";
export type DeployStatus = "queued" | "running" | "succeeded" | "failed" | "superseded" | "interrupted";
export type DeployStage = "fetch" | "config" | "policy" | "build" | "up" | "health";
export type DeployFailureReason =
  | "fetch_failed"
  | "compose_not_found"
  | "compose_invalid"
  | "policy_violation"
  | "build_failed"
  | "up_failed"
  | "health_failed"
  | "runtime_unavailable"
  | "domain_conflict"
  | "internal";

export interface DeployFailure {
  reason: DeployFailureReason;
  detail: string;
  violations?: PolicyViolation[];
}

export interface Deploy {
  id: number;
  project: string;
  ref: string;
  commit: string;
  source: DeploySource;
  status: DeployStatus;
  stage: DeployStage;
  error?: DeployFailure;
  created_at: Timestamp;
  started_at: Timestamp | null;
  finished_at: Timestamp | null;
  /** Added with rollback support; absent on older agents. */
  rollback_of?: number;
}

/** data of a deploys.update notification. */
export type DeployUpdate =
  | { type: "log"; seq?: number; stream: "stdout" | "stderr" | "agent"; text: string }
  | { type: "status"; deploy: Deploy };

/** data of a -32012 error. */
export interface DeployErrorData {
  reason: "project_not_ready" | "ref_not_allowed" | "ref_not_found" | "no_rollback_target" | (string & {});
  detail: string;
}

/** data of a -32010 error. */
export interface RepositoryErrorData {
  reason:
    | "invalid_url"
    | "unreachable"
    | "auth_failed"
    | "not_found"
    | "ref_not_found"
    | "compose_not_found"
    | "failed"
    | (string & {});
  detail: string;
}

// -------------------------------------------------------------------- edge

export interface EdgeStatus {
  enabled: boolean;
  state: "disabled" | "starting" | "running" | "error";
  last_error?: string;
  image: string;
  http_port: number;
  https_port: number;
  server_hostnames: string[];
  self_signed: { fingerprint_sha256: string; not_after: Timestamp };
  certificates: Array<{ hostname: string; source: "acme" | "installed"; not_after?: Timestamp }>;
  routes: number;
}

export interface EdgeCSRResult {
  hostname: string;
  csr: string;
}

export interface EnrollmentAuthorization {
  user_code: string;
  verification_uri: string;
  verification_uri_complete?: string;
  expires_at: Timestamp;
}

// ------------------------------------------------------- auth, users, tokens

export interface Principal {
  kind: "user" | "token" | "local" | "control-plane";
  name: string;
  role: Role;
}

export interface User {
  username: string;
  role: Role;
  status: "pending" | "active" | "disabled";
  totp_enrolled: boolean;
  created_at: Timestamp;
  last_login_at?: Timestamp;
}

export interface SetupLink {
  user: User;
  setup_url: string;
  fingerprint_sha256?: string;
  expires_at: Timestamp;
}

export interface CreatedToken {
  token: string;
  name: string;
  role: Role;
  expires_at?: Timestamp;
}

export interface TokenInfo {
  name: string;
  role: Role;
  created_at: Timestamp;
  created_by: string;
  expires_at?: Timestamp;
  last_used_at?: Timestamp;
  revoked?: boolean;
}

export interface AuditEntry {
  id: number;
  time: Timestamp;
  principal_kind: string;
  principal_name: string;
  ip: string;
  action: string;
  target: string;
  result: string;
}

// --------------------------------------------------------------- method map

/** Name to params/result for every request method of the spec. */
export interface MethodMap {
  "agent.hello": { params: HelloParams; result: HelloResult };
  "agent.info": { params: undefined; result: AgentInfo };
  "agent.ping": { params: undefined; result: PingResult };
  "agent.heartbeat.subscribe": {
    params: { interval_ms?: number } | undefined;
    result: { subscription: SubscriptionID; interval_ms: number };
  };
  "agent.heartbeat.unsubscribe": { params: { subscription: SubscriptionID }; result: Record<string, never> };
  "runtime.info": { params: undefined; result: RuntimeInfo };
  "containers.list": { params: { project?: string } | undefined; result: Container[] };
  "containers.inspect": { params: { id: string }; result: ContainerDetail };
  "containers.start": { params: { id: string }; result: Record<string, never> };
  "containers.stop": { params: { id: string; timeout_s?: number }; result: Record<string, never> };
  "containers.restart": { params: { id: string; timeout_s?: number }; result: Record<string, never> };
  "containers.remove": { params: { id: string; force?: boolean }; result: Record<string, never> };
  "containers.logs.subscribe": {
    params: { id: string; tail?: number; since?: Timestamp; timestamps?: boolean };
    result: { subscription: SubscriptionID };
  };
  "containers.logs.unsubscribe": { params: { subscription: SubscriptionID }; result: Record<string, never> };
  "projects.create": { params: ProjectCreateParams; result: { project: Project; webhook_secret: string } };
  "projects.get": { params: { slug: string }; result: Project };
  "projects.list": { params: undefined; result: Project[] };
  "projects.update": { params: ProjectUpdateParams; result: { project: Project; webhook_secret?: string } };
  "projects.verify": { params: { slug: string }; result: Project };
  "projects.delete": { params: { slug: string; volumes?: boolean }; result: PruneReport };
  "projects.env.set": { params: { slug: string; vars: Record<string, string> }; result: EnvNames };
  "projects.env.unset": { params: { slug: string; names: string[] }; result: EnvNames };
  "projects.env.list": { params: { slug: string }; result: { vars: EnvVarInfo[] } };
  "projects.expose": {
    params: { slug: string; service: string; port: number; domains: string[]; path?: string };
    result: Project;
  };
  "projects.services": { params: { slug: string }; result: { services: ProjectService[] } };
  "projects.services.start": { params: { slug: string; service: string }; result: ProjectService };
  "projects.services.stop": { params: { slug: string; service: string }; result: ProjectService };
  "projects.services.restart": { params: { slug: string; service: string }; result: ProjectService };
  "projects.unexpose": { params: { slug: string; service: string }; result: Project };
  "deploys.trigger": { params: { project: string; ref?: string }; result: Deploy };
  "deploys.rollback": { params: { project: string; deploy?: number }; result: Deploy };
  "deploys.get": { params: { id: number }; result: Deploy };
  "deploys.list": { params: { project: string; limit?: number; before_id?: number }; result: Deploy[] };
  "deploys.subscribe": { params: { id: number; since_seq?: number }; result: { subscription: SubscriptionID } };
  "deploys.unsubscribe": { params: { subscription: SubscriptionID }; result: Record<string, never> };
  "host.stats": { params: { history?: boolean; per_core?: boolean } | undefined; result: HostStatsResult };
  "host.subscribe": {
    params: { interval_ms?: number; per_core?: boolean } | undefined;
    result: { subscription: SubscriptionID; interval_ms: number };
  };
  "host.unsubscribe": { params: { subscription: SubscriptionID }; result: Record<string, never> };
  "containers.stats": {
    params: { project?: string; id?: string; history?: boolean } | undefined;
    result: ContainerStatsResult;
  };
  "containers.stats.subscribe": {
    params: { project?: string; id?: string; interval_ms?: number } | undefined;
    result: { subscription: SubscriptionID; interval_ms: number };
  };
  "containers.stats.unsubscribe": { params: { subscription: SubscriptionID }; result: Record<string, never> };
  "edge.status": { params: undefined; result: EdgeStatus };
  "edge.csr": { params: { hostname: string }; result: EdgeCSRResult };
  "edge.certificate.install": { params: { hostname: string; certificate: string }; result: EdgeStatus };
  "edge.certificate.remove": { params: { hostname: string }; result: EdgeStatus };
  "enrollment.start": { params: { url?: string; force?: boolean } | undefined; result: EnrollmentAuthorization };
  "enrollment.wait": { params: undefined; result: { control_plane_url: string } };
  "enrollment.remove": { params: undefined; result: Record<string, never> };
  "auth.whoami": { params: undefined; result: Principal };
  "users.list": { params: undefined; result: User[] };
  "users.create": { params: { username: string; role: Role }; result: SetupLink };
  "users.update": { params: { username: string; role?: Role; disabled?: boolean }; result: User };
  "users.reset": { params: { username: string }; result: SetupLink };
  "users.delete": { params: { username: string }; result: Record<string, never> };
  "panel.setup": { params: { username?: string } | undefined; result: SetupLink };
  "tokens.create": { params: { name: string; role: Role; expires_in?: string }; result: CreatedToken };
  "tokens.list": { params: undefined; result: TokenInfo[] };
  "tokens.revoke": { params: { name: string }; result: Record<string, never> };
  "audit.list": { params: { limit?: number; before_id?: number } | undefined; result: AuditEntry[] };
}

export type MethodName = keyof MethodMap;

/** JSON-RPC error codes of the protocol. */
export const ErrorCode = {
  Parse: -32700,
  InvalidRequest: -32600,
  MethodNotFound: -32601,
  InvalidParams: -32602,
  Internal: -32603,
  Unauthorized: -32001,
  HandshakeRequired: -32002,
  IncompatibleVersion: -32003,
  SubscriptionNotFound: -32004,
  Overloaded: -32005,
  NotFound: -32006,
  PolicyViolation: -32007,
  RuntimeUnavailable: -32008,
  AlreadyExists: -32009,
  Repository: -32010,
  Enrollment: -32011,
  Deploy: -32012,
  /** Client-side: the call timed out. */
  Timeout: -1,
  /** Client-side: the connection was lost or closed. */
  ConnectionLost: -2,
} as const;
