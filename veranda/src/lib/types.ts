export interface LocalProject {
    id: string;
    name: string;
}
export interface LocalYard {
    name: string;
    kind: 'container' | 'vm';
    state: string;
    projects: LocalProject[];
}
export interface LocalOwnerHost {
    id: string;
    yards: LocalYard[];
}
export interface LocalFleetSnapshot {
    verandaVersion?: string;
    connectionId?: string | null;
    capabilities?: string[];
    engineVersion: string;
    observedAt: string;
    currentYardName: string;
    owner: LocalOwnerHost;
}
export interface LocalFleetError {
    code: string;
    message: string;
}
export type FleetLoader = () => Promise<LocalFleetSnapshot>;
export type Confirmation = 'never' | 'prompt-default-yes' | 'prompt-default-no';
export interface ConnectionRecord {
    id: string;
    hostId: string;
    destination: string;
    authLabel?: string;
    state?: 'connected' | 'disconnected' | 'incompatible' | 'stale';
}
export interface ConnectionAssessment {
    assessmentId: string;
    destination: string;
    fingerprint: string;
    confirmation: Confirmation;
    consequences: string[];
    previousFingerprint?: string;
}
export interface RemovalAssessment {
    assessmentId: string;
    confirmation: Confirmation;
    consequences: string[];
    connection: ConnectionRecord;
}
export interface Provenance {
    scope: string;
    role: string;
    status: string;
    value?: unknown;
}
export interface Profile {
    hasYardPreset?: boolean;
    name: string;
    description?: string;
    selected: boolean;
    provisionable: boolean;
    provisionScope: string;
    eligible: boolean;
    eligibility: string;
    convergence: 'unknown' | 'not-applicable' | 'current' | 'changes-required';
    resources: string[];
}
export interface ProfileList {
    yardName: string;
    selection: {
        value: string;
        provenance: Provenance[];
    };
    profiles: Profile[];
}
export interface Setting {
    name: string;
    kind: string;
    type: string;
    value?: unknown;
    valueAvailable: boolean;
    editable: boolean;
    provenance: Provenance[];
    enum?: string[];
    minimum?: number;
    maximum?: number;
    optional?: boolean;
}
export interface SettingsList {
    yardName?: string;
    settings: Setting[];
}
export interface CredentialPeer {
    name: string;
    role: string;
    manualOnly: boolean;
    trusted: boolean;
    lastAttempt: number;
    lastSuccess: number;
    consecutiveFailures: number;
    nextRetry: number;
    failed: boolean;
}
export interface HostSyncStatus {
    schemaVersion: number;
    hostId: string;
    hostIdPending: boolean;
    automation: 'manual';
    offline: boolean;
    registration: string;
    recoveryRequired: boolean;
    generation?: number;
    appliedCommit?: string;
    git?: {
        available: boolean;
        remote: string;
        branch?: string;
        upstream?: string;
        head?: string;
        relation?: string;
        worktree?: string;
        staged?: number;
        unstaged?: number;
        untracked?: number;
        conflicts?: number;
        ahead?: number;
        behind?: number;
        lastFetch?: string;
    };
    credentials: {
        state: string;
        records: number;
        conflicts: number;
        peers: CredentialPeer[];
    };
}
export interface DiagnosticFact {
    label: string;
    value: string;
}
export interface YardDetails {
    selection?: { value: string; provenance: Provenance[] };
    profiles: Profile[];
    settings: Setting[];
    diagnostics: DiagnosticFact[];
    capabilities: string[];
}
export interface HostDetails {
    profiles?: Profile[];
    sync: HostSyncStatus | null;
    settings: Setting[];
    diagnostics: DiagnosticFact[];
    capabilities: string[];
}
export interface OperationStep {
    id: string;
    target: string;
    observed: string;
    desired: string;
    decision: string;
    preconditions: string[];
    dependsOn: string[];
    verify: string;
    consequence?: string;
}
export interface OperationPlan {
    planId: string;
    digest: string;
    operationId: string;
    summary: string;
    confirmation: Confirmation;
    consequences: string[];
    expiresAt: string;
    steps: OperationStep[];
}
export interface VerandaEvent {
    snapshot?: LocalFleetSnapshot;
    yard?: string;
    connectionId?: string | null;
    operationId?: string;
    state: string;
    message?: string;
}
export interface VerandaApi {
    markReady?(fleet: { owners: number; yards: number; projects: number }): Promise<void>;
    listConnections(): Promise<ConnectionRecord[]>;
    loadFleet(args: {
        connectionId: string | null;
    }): Promise<LocalFleetSnapshot>;
    assessConnection(args: {
        destination: string;
    }): Promise<ConnectionAssessment>;
    cancelAssessment(args: { assessmentId: string }): Promise<void>;
    connectRemote(args: {
        assessmentId: string;
        confirmed: boolean;
        fingerprint: string;
    }): Promise<ConnectionRecord>;
    repairConnection(args: {
        connectionId: string;
    }): Promise<ConnectionAssessment>;
    assessRemoval(args: {
        connectionId: string;
    }): Promise<RemovalAssessment>;
    removeConnection(args: {
        assessmentId: string;
        confirmed: boolean;
    }): Promise<void>;
    loadYard(args: {
        connectionId: string | null;
        yard: string;
    }): Promise<YardDetails>;
    loadHost(args: {
        connectionId: string | null;
    }): Promise<HostDetails>;
    planOperation(args: {
        connectionId: string | null;
        yard: string | null;
        command: string;
        arguments: string[];
    }): Promise<OperationPlan>;
    executeOperation(args: {
        planId: string;
        digest: string;
        confirmed: boolean;
    }): Promise<{
        operationId: string;
    }>;
    discardOperation(args: {
        planId: string;
    }): Promise<void>;
    cancelOperation(args: {
        operationId: string;
    }): Promise<void>;
    launchSession(args: {
        connectionId: string | null;
        yard: string | null;
        projectId: string | null;
        kind: 'shell' | 'vscode' | 'resources';
    }): Promise<{
        command?: string;
    }>;
    shellCommand(args: {connectionId: string | null; yard: string | null; projectId: string | null}): Promise<{command: string}>;
    listen(handler: (event: VerandaEvent) => void): Promise<() => void>;
}
