#!/usr/bin/env python3
"""Sanitized owner-resource records; transport credentials never enter this module."""
import hashlib
import json

import records

COUNTERS = ('cpu_ns', 'memory_bytes', 'memory_peak_bytes', 'swap_bytes',
            'io_read_bytes', 'io_write_bytes', 'io_read_ops', 'io_write_ops', 'rss_bytes', 'pss_bytes', 'rss_peak_bytes')


def nullable_time(value):
    return None if value is None or value.startswith('0001-01-01') else value


def memory_bytes(value):
    import re
    match = re.fullmatch(r'([1-9][0-9]*)(MiB|GiB)', value or '')
    return int(match[1]) * (1024**2 if match[2] == 'MiB' else 1024**3) if match else None


def object_id(kind, owner, slot, generation, epoch, selector):
    identity = json.dumps([kind, owner, slot, generation, epoch, selector], separators=(',', ':'))
    return 'allocation:' + hashlib.sha256(identity.encode()).hexdigest()


def base(config, resource, selector=None):
    generation = resource.get('resource_generation', resource.get('generation'))
    slot = resource.get('slot_id')
    if type(generation) is not int or generation <= 0 or not isinstance(slot, str):
        raise ValueError('invalid allocation identity')
    epoch = resource.get('lease_epoch')
    if config['kind'] == 'vm' and (type(epoch) is not int or epoch <= 0):
        raise ValueError('invalid allocation epoch')
    selector = selector or resource.get('selector', '1' if config['kind'] == 'vm' else 'emulator')
    return dict(object_id=object_id(config['kind'], config['owner_id'], slot, generation, epoch, selector),
                object_kind=config['kind'], owner_id=config['owner_id'], slot_id=slot,
                resource_generation=generation, lease_epoch=epoch, resource_id=selector,
                selector=selector)


def allocation(config, resource, selector=None):
    body = base(config, resource, selector)
    owner = resource.get('owner', resource.get('context')) or {}
    body.update(state=resource.get('state', resource.get('to_state')), correlation_id=owner.get('run'),
                yard_id=owner.get('yard'), project_id=owner.get('project'), purpose=owner.get('purpose'),
                acquired_at=nullable_time(resource.get('acquired_at')), ready_at=nullable_time(resource.get('ready_at')),
                expires_at=nullable_time(resource.get('expires_at')))
    environment = resource.get('environment') or {}
    body['reservation_semantics'] = 'allocated_virtual_capacity_held'
    if config['kind'] == 'vm':
        memory = memory_bytes(environment.get('memory_per_vm'))
        disk = memory_bytes(environment.get('disk_per_vm'))
        cpu = environment.get('cpu_per_vm')
        body.update(requested_cpu=cpu, requested_memory_bytes=memory, requested_disk_bytes=disk,
                    allocated_cpu=cpu, allocated_memory_bytes=memory, allocated_disk_bytes=disk)
    else:
        request = resource.get('request') or {}
        # The native owner reports the enforced runtime memory ceiling (including
        # overhead). Missing guest RAM/disk requests are not inferred from device names.
        body.update(requested_cpu=resource.get('requested_cpu'),
                    requested_memory_bytes=request.get('memory_bytes'), requested_disk_bytes=request.get('disk_bytes'),
                    allocated_cpu=resource.get('allocated_cpu'),
                    allocated_memory_bytes=resource.get('reserved_memory_bytes'),
                    allocated_disk_bytes=resource.get('allocated_disk_bytes'))
    body.update(reserved_cpu=body['allocated_cpu'], reserved_memory_bytes=body['allocated_memory_bytes'],
                reserved_disk_bytes=body['allocated_disk_bytes'])
    return body


def association(body, run, acquired_at, stopped_at, source):
    identity = json.dumps([body['object_id'], run['agent_run_id']], separators=(',', ':'))
    return dict(association_id='association:' + hashlib.sha256(identity.encode()).hexdigest(),
                object_id=body['object_id'], agent_run_id=run['agent_run_id'],
                start_time=nullable_time(acquired_at), end_time=stopped_at,
                quality='explicit', shared=False, source=source)


def emit_sample(data_dir, config, native, event_id=None):
    body = base(config, native)
    for name in COUNTERS:
        value = native.get(name)
        if value is not None and (type(value) is not int or not 0 <= value < 2**64):
            raise ValueError('invalid resource counter')
        body[name] = value
    body.update(boot_id=native.get('boot_id'), monotonic_ns=native.get('monotonic_ns'),
                counter_generation=native.get('counter_generation'), phase=native.get('phase', 'periodic'),
                final_complete=False, missing=[name for name in COUNTERS if body[name] is None],
                quality='partial' if native.get('missing') or not native.get('counter_generation') else 'measured',
                accounting=native.get('accounting', 'inclusive-owner-resource'))
    if event_id is None:
        event_id = 'sample-' + hashlib.sha256(json.dumps([body['object_id'], native.get('source_time'),
                  native.get('monotonic_ns'), native.get('counter_generation')], separators=(',', ':')).encode()).hexdigest()
    admitted = records.emit(data_dir, 'sample', body, source_id=config['source_id'],
                            source_time=native.get('source_time'), event_id=event_id)
    for binding in config.get('associations', {}).values():
        if (binding.get('slot_id') == body['slot_id'] and binding.get('resource_generation') == body['resource_generation']
                and binding.get('lease_epoch') == body['lease_epoch']):
            edge = association(body, binding, native.get('acquired_at'), None, 'validated-grant')
            admitted = records.emit(data_dir, 'association', edge, source_id=config['source_id'],
                                    source_time=native.get('source_time'), event_id=event_id + '-association') and admitted
    return admitted


def emit_event(data_dir, config, event):
    identity = event['event_id']
    resource = event.get('allocation', event)
    source_time = event.get('source_time', event.get('timestamp'))
    samples = event.get('observations') or []
    count = (resource.get('environment') or {}).get('vm_count', 1)
    if type(count) is not int or not 1 <= count <= 2:
        raise ValueError('invalid resource count')
    selectors = [str(index) for index in range(1, count + 1)] if config['kind'] == 'vm' else ['emulator']
    ok = True
    for selector in selectors:
        body = allocation(config, resource, selector)
        body['state'] = event.get('to_state') or resource.get('state')
        phase = event.get('phase', event.get('kind'))
        terminal = phase in ('stopped', 'lease.available') or event.get('to_state') == 'available'
        body['state'] = 'available' if terminal else (event.get('to_state') or (resource.get('lease') or {}).get('state') or resource.get('state'))
        body['phase'] = phase
        if terminal:
            body['stopped_at'] = source_time
        suffix = str(selector or 'allocation')
        ok = records.emit(data_dir, 'allocation', body, source_id=config['source_id'],
                          source_time=source_time, event_id=identity + '-allocation-' + suffix) and ok
        correlation = body.get('correlation_id')
        run = config.get('associations', {}).get(correlation)
        if run and any(run.get(name) != body.get(name) for name in ('slot_id', 'resource_generation', 'lease_epoch')):
            ok = records.emit(data_dir, 'gap', dict(object_id=body['object_id'], reason='correlation_generation_mismatch',
                              quality='unknown'), source_id=config['source_id'], source_time=source_time,
                              event_id=identity+'-binding-mismatch-'+suffix) and ok
            run = None
        if run:
            edge = association(body, run, body.get('acquired_at'), body.get('stopped_at'), 'acquire-correlation')
            ok = records.emit(data_dir, 'association', edge, source_id=config['source_id'],
                              source_time=source_time, event_id=identity + '-association-' + suffix) and ok
        if terminal:
            ok = records.emit(data_dir, 'gap', dict(object_id=body['object_id'], reason='final_tail_missing',
                              end_time=source_time, quality='unknown'), source_id=config['source_id'],
                              source_time=source_time, event_id=identity + '-tail-' + suffix) and ok
    for index, sample in enumerate(samples):
        ok = emit_sample(data_dir, config, dict(sample, acquired_at=resource.get('acquired_at')),
                         identity + '-sample-' + str(index)) and ok
    return ok
