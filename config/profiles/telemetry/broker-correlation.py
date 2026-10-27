#!/usr/bin/env python3
"""Optional bounded local association hook; native lease requests stay unchanged."""
import json
import os
import sys

import records
import sources


def register(data_dir, agent_run, request):
    try:
        if not isinstance(agent_run, str) or not records.IDENTITY.fullmatch(agent_run):
            raise ValueError('invalid agent run')
        request = sources.registration_request(request)
        # The intent is durable before any reader lock or analytics delivery.
        if request['phase'] == 'held':
            return sources.store_intent(data_dir, agent_run, request)
        public = {key: request.get(key) for key in ('owner_id', 'yard_id', 'project_id', 'correlation_id')}
        public.update(agent_run_id=agent_run, source='acquire-correlation', quality='explicit')
        admitted = records.emit(data_dir, 'relation', public)
        return admitted
    except (ValueError, TypeError):
        sources.registration_gap(data_dir, 'correlation_registration_invalid')
        return False
    except (OSError, KeyError):
        sources.registration_gap(data_dir, 'correlation_registration_unavailable')
        return False


def main():
    try:
        raw = sys.stdin.buffer.read(16385)
        if len(raw) > 16384:
            sources.registration_gap(os.environ['SUBYARD_TELEMETRY_DATA_DIR'], 'correlation_registration_invalid')
            return 0
        register(os.environ['SUBYARD_TELEMETRY_DATA_DIR'], os.environ['SUBYARD_AGENT_RUN_ID'], json.loads(raw))
    except (OSError, ValueError, KeyError, TypeError):
        if os.environ.get('SUBYARD_TELEMETRY_DATA_DIR'):
            sources.registration_gap(os.environ['SUBYARD_TELEMETRY_DATA_DIR'], 'correlation_registration_invalid')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
