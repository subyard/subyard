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
