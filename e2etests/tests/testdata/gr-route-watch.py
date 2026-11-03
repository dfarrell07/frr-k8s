#!/usr/bin/env python3
"""Passive rtnetlink events and exact-prefix kernel-route sampling; JSONL stdout.

Wait for a ready record before triggering restart. Presence samples are complete
route dumps, not atomic kernel transactions; their start/end times bound each
observation. No event or incomplete/failed dump is treated as proof of absence.
An observed matching route deletion vetoes qualification, without claiming whole
prefix absence or that every possible deletion was observed. Nominal 10ms
cadence statistics are supplemental; successful-dump coverage must stay <1s.
Exit: 0 complete clean window, 1 absence/error/incomplete, 2 baseline/fatal error.
"""
import argparse
import collections
import errno
import ipaddress
import json
import signal
import socket
import struct
import sys
import threading
import time

NLMSG_ERROR, NLMSG_DONE, NLMSG_OVERRUN = 2, 3, 4
NLM_F_REQUEST, NLM_F_DUMP, NLM_F_DUMP_INTR = 1, 0x300, 0x10
NAMES = {16: "NEWLINK", 17: "DELLINK", 20: "NEWADDR", 21: "DELADDR",
         24: "NEWROUTE", 25: "DELROUTE", 28: "NEWNEIGH", 29: "DELNEIGH",
         104: "NEWNEXTHOP", 105: "DELNEXTHOP"}
ROUTE_ATTRS = {1: "dst", 2: "src", 3: "iif", 4: "oif", 5: "gateway",
               6: "priority", 7: "prefsrc", 8: "metrics", 9: "multipath",
               15: "table", 18: "via", 30: "nh_id"}
NH_ATTRS = {1: "id", 2: "group", 3: "group_type", 4: "blackhole",
            5: "oif", 6: "gateway", 10: "master"}
LOCK = threading.Lock()


def stamp():
    return {"monotonic_ns": time.monotonic_ns(), "realtime_ns": time.time_ns()}


def emit(kind, **fields):
    with LOCK:
        print(json.dumps({"kind": kind, **stamp(), **fields}, separators=(",", ":")),
              flush=True)


def aligned(n):
    return (n + 3) & ~3


def attrs(data):
    result = []
    offset = 0
    while offset + 4 <= len(data):
        length, raw_type = struct.unpack_from("=HH", data, offset)
        if length < 4 or offset + length > len(data):
            raise ValueError("malformed netlink attribute")
        result.append((raw_type, data[offset + 4:offset + length]))
        offset += aligned(length)
    if offset < len(data) and any(data[offset:]):
        raise ValueError("trailing netlink attribute bytes")
    return result


def address(data, family):
    size = {socket.AF_INET: 4, socket.AF_INET6: 16}.get(family)
    if size and len(data) == size:
        return socket.inet_ntop(family, data)
    return data.hex()


def u32(data):
    return struct.unpack("=I", data)[0] if len(data) == 4 else data.hex()


def multipath(data, family):
    result, offset = [], 0
    while offset + 8 <= len(data):
        length, flags, hops, oif = struct.unpack_from("=HBBi", data, offset)
        if length < 8 or offset + length > len(data):
            raise ValueError("malformed multipath nexthop")
        result.append({"flags": flags, "hops": hops, "oif": oif,
                       "attrs": decode_attrs(data[offset + 8:offset + length],
                                             family, "route")})
        offset += aligned(length)
    return result


def decode_attrs(data, family, category):
    names = ROUTE_ATTRS if category == "route" else NH_ATTRS if category == "nh" else {}
    result = []
    for raw_type, value in attrs(data):
        key = raw_type & 0x3fff
        decoded = value.hex()
        if category == "route":
            if key in (1, 2, 5, 7):
                decoded = address(value, family)
            elif key in (3, 4, 6, 15, 30):
                decoded = u32(value)
            elif key == 9:
                decoded = multipath(value, family)
            elif key == 18 and len(value) >= 2:
                via_family = struct.unpack_from("=H", value)[0]
                decoded = {"family": via_family, "address": address(value[2:], via_family)}
        elif category == "nh":
            if key in (1, 5, 10):
                decoded = u32(value)
            elif key == 6:
                decoded = address(value, family)
            elif key == 2:
                decoded = [{"id": nhid, "weight": weight}
                           for nhid, weight, _, _ in struct.iter_unpack("=IBBH", value)]
            elif key == 3 and len(value) == 2:
                decoded = struct.unpack("=H", value)[0]
        result.append({"type": key, "raw_type": raw_type, "name": names.get(key),
                       "raw": value.hex(), "value": decoded})
    return result


def decode(msg_type, data):
    if msg_type in (24, 25, 26):
        family, dst_len, src_len, tos, table, protocol, scope, route_type, flags = struct.unpack_from("=BBBBBBBBI", data)
        attributes = decode_attrs(data[12:], family, "route")
        values = {a["type"]: a["value"] for a in attributes}
        dst = values.get(1, "::" if family == socket.AF_INET6 else "0.0.0.0")
        return {"family": family, "prefix": str(ipaddress.ip_network(f"{dst}/{dst_len}", strict=False)),
                "dst_len": dst_len, "src_len": src_len, "tos": tos,
                "table": values.get(15, table), "protocol": protocol,
                "scope": scope, "route_type": route_type, "flags": flags,
                "attrs": attributes}
    if msg_type in (104, 105, 106):
        family, scope, protocol, _, flags = struct.unpack_from("=BBBBI", data)
        return {"family": family, "scope": scope, "protocol": protocol,
                "flags": flags, "attrs": decode_attrs(data[8:], family, "nh")}
    if msg_type in (16, 17, 18):
        family, _, link_type, index, flags, change = struct.unpack_from("=BBHiII", data)
        header = {"family": family, "link_type": link_type, "index": index,
                  "flags": flags, "change": change}
        offset = 16
    elif msg_type in (20, 21, 22):
        family, prefixlen, flags, scope, index = struct.unpack_from("=BBBBI", data)
        header = {"family": family, "prefixlen": prefixlen, "flags": flags,
                  "scope": scope, "index": index}
        offset = 8
    elif msg_type in (28, 29, 30):
        family, _, _, index, state, flags, ndtype = struct.unpack_from("=BBHiHBB", data)
        header = {"family": family, "index": index, "state": state,
                  "flags": flags, "neighbor_type": ndtype}
        offset = 12
    else:
        return {"raw": data.hex()}
    return {**header, "attrs": decode_attrs(data[offset:], family, "other")}


def messages(data):
    offset = 0
    while offset + 16 <= len(data):
        length, msg_type, flags, seq, pid = struct.unpack_from("=IHHII", data, offset)
        if length < 16 or offset + length > len(data):
            raise ValueError("malformed netlink message")
        yield msg_type, flags, seq, pid, data[offset + 16:offset + length]
        offset += aligned(length)
    if offset < len(data) and any(data[offset:]):
        raise ValueError("trailing netlink message bytes")


def receive(sock):
    data, _, flags, _ = sock.recvmsg(1 << 20)
    received = stamp()
    if flags & socket.MSG_TRUNC:
        raise OSError(errno.ENOBUFS, "truncated netlink datagram")
    return received, messages(data)


def netlink_socket(groups=0):
    sock = socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, socket.NETLINK_ROUTE)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4 << 20)
    sock.bind((0, groups))
    sock.settimeout(0.25)
    return sock


class Watch:
    def __init__(self, args):
        self.args = args
        self.stop = threading.Event()
        self.loss = False
        self.absence = False
        self.query_errors = 0
        self.sequence = 0
        self.termination_signal = None
        self.termination_monotonic_ns = None
        # LINK, NEIGH, IPv4/IPv6 IFADDR and ROUTE. NEXTHOP uses membership API.
        self.events = netlink_socket(1 | 4 | 0x10 | 0x40 | 0x100 | 0x400)
        self.events.setsockopt(270, 1, struct.pack("=I", 32))  # RTNLGRP_NEXTHOP
        self.query = netlink_socket()
        self.query.settimeout(1.0)

    def event_loop(self):
        index = 0
        while not self.stop.is_set():
            try:
                received, batch = receive(self.events)
                for msg_type, flags, seq, pid, payload in batch:
                    index += 1
                    if msg_type in (NLMSG_OVERRUN, NLMSG_ERROR):
                        self.loss = True
                    decoded = decode(msg_type, payload)
                    if (msg_type == 25 and decoded["table"] == self.args.table
                            and decoded["prefix"] in self.args.prefixes
                            and (self.args.protocol is None
                                 or decoded["protocol"] == self.args.protocol)):
                        self.target_withdrawals += 1
                    emit("netlink_event", received=received, event_index=index,
                         message=NAMES.get(msg_type, str(msg_type)), msg_type=msg_type,
                         nl_flags=flags, nl_seq=seq, nl_pid=pid,
                         data=decoded)
            except socket.timeout:
                continue
            except (OSError, ValueError, struct.error) as exc:
                self.loss = True
                emit("observer_error", error=str(exc), errno=getattr(exc, "errno", None))
                if not isinstance(exc, OSError) or exc.errno != errno.ENOBUFS:
                    self.stop.set()

    def snapshot(self):
        self.sequence += 1
        seq = self.sequence
        request = struct.pack("=BBBBBBBBI", self.args.family, 0, 0, 0, 0, 0, 0, 0, 0)
        header = struct.pack("=IHHII", 16 + len(request), 26, NLM_F_REQUEST | NLM_F_DUMP, seq, 0)
        started = stamp()
        self.query.send(header + request)
        routes = {prefix: [] for prefix in self.args.prefixes}
        deadline = time.monotonic() + 2.0
        while time.monotonic() < deadline:
            _, batch = receive(self.query)
            for msg_type, flags, reply_seq, _, payload in batch:
                if reply_seq != seq:
                    continue
                if flags & NLM_F_DUMP_INTR:
                    raise RuntimeError("route dump interrupted; state is unknown")
                if msg_type == NLMSG_ERROR:
                    code = struct.unpack_from("=i", payload)[0]
                    if code:
                        raise OSError(-code, "GETROUTE failed")
                elif msg_type == NLMSG_DONE:
                    if len(payload) >= 4 and struct.unpack_from("=i", payload)[0]:
                        raise RuntimeError("route dump completion reported an error")
                    return {"started": started, "completed": stamp(), "routes": routes}
                elif msg_type == NLMSG_OVERRUN:
                    raise OSError(errno.ENOBUFS, "query socket overrun")
                elif msg_type == 24:
                    route = decode(msg_type, payload)
                    if route["table"] == self.args.table and route["prefix"] in routes:
                        if self.args.protocol is None or route["protocol"] == self.args.protocol:
                            routes[route["prefix"]].append(route)
        raise TimeoutError("GETROUTE did not complete within two seconds")

    def run(self):
        self.target_withdrawals = 0
        emit("started", prefixes=self.args.prefixes, table=self.args.table,
             protocol=self.args.protocol, poll_ms=self.args.poll_ms,
             duration_seconds=self.args.duration,
             event_rcvbuf=self.events.getsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF),
             query_rcvbuf=self.query.getsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF))
        thread = threading.Thread(target=self.event_loop, daemon=True)
        thread.start()
        intervals = collections.deque(maxlen=100000)
        durations = collections.deque(maxlen=100000)
        count, interval_total, interval_max, interval_min = 0, 0, 0, None
        missed_deadlines = terminal_missed_slots = 0
        last_start = last_complete = end = None
        last_success_started, max_success_gap_ns = None, 0
        ready, monitoring_finished, window_complete = None, None, False
        try:
            baseline = self.snapshot()
            last_success_started = baseline["started"]["monotonic_ns"]
            max_success_gap_ns = baseline["completed"]["monotonic_ns"] - last_success_started
            emit("baseline", **baseline)
            previous = {p: bool(r) for p, r in baseline["routes"].items()}
            if not all(previous.values()) or self.loss or self.stop.is_set():
                emit("baseline_not_ready", present=previous, observer_loss=self.loss)
                return 2
            ready = time.monotonic_ns()
            emit("ready", present=previous, observer_loss=self.loss)
            end = ready + int(self.args.duration * 1e9)
            period = int(self.args.poll_ms * 1e6)
            next_poll, next_strict, last_start = ready, ready, None
            while not self.stop.is_set() and time.monotonic_ns() < end:
                now = time.monotonic_ns()
                if now < next_poll:
                    self.stop.wait((next_poll - now) / 1e9)
                    continue
                sample_start = time.monotonic_ns()
                if last_start is not None:
                    interval = sample_start - last_start
                    intervals.append(interval)
                    interval_total += interval
                    interval_min = interval if interval_min is None else min(interval_min, interval)
                    interval_max = max(interval_max, interval)
                last_start = sample_start
                count += 1
                try:
                    sample = self.snapshot()
                    last_complete = sample["completed"]["monotonic_ns"]
                    max_success_gap_ns = max(max_success_gap_ns, last_complete - last_success_started)
                    last_success_started = sample["started"]["monotonic_ns"]
                    durations.append(sample["completed"]["monotonic_ns"] - sample["started"]["monotonic_ns"])
                    present = {p: bool(r) for p, r in sample["routes"].items()}
                    self.absence |= not all(present.values())
                    for prefix in present:
                        if present[prefix] != previous[prefix]:
                            emit("presence_transition", prefix=prefix, present=present[prefix],
                                 started=sample["started"], completed=sample["completed"],
                                 routes=sample["routes"][prefix], complete_dump=True)
                    previous = present
                    if time.monotonic_ns() >= next_strict:
                        emit("strict_check", all_present=all(present.values()), present=present,
                             started=sample["started"], completed=sample["completed"],
                             observer_loss=self.loss, query_errors=self.query_errors)
                        next_strict = time.monotonic_ns() + 1_000_000_000
                except (OSError, RuntimeError, ValueError, TimeoutError, struct.error) as exc:
                    self.query_errors += 1
                    emit("query_error", error=str(exc), errno=getattr(exc, "errno", None),
                         state="unknown", complete_dump=False)
                next_poll += period
                now = time.monotonic_ns()
                if next_poll < end and next_poll < now:
                    missed_deadlines += 1
                    next_poll = now  # no burst of catch-up queries
            monitoring_finished = time.monotonic_ns()
            # A pause in the final wait can bypass the loop's normal deadline
            # accounting. Count every still-scheduled slot before the intended
            # end; waking late after the normal end-boundary sleep is harmless.
            if monitoring_finished >= end and next_poll < end:
                terminal_missed_slots = (end - next_poll + period - 1) // period
                missed_deadlines += terminal_missed_slots
                emit("cadence_degraded", reason="nominal terminal poll slots were skipped",
                     terminal_missed_poll_slots=terminal_missed_slots,
                     next_poll_monotonic_ns=next_poll, window_end_monotonic_ns=end,
                     last_sample_started_monotonic_ns=last_start,
                     last_sample_completed_monotonic_ns=last_complete)
            max_success_gap_ns = max(max_success_gap_ns, end - last_success_started)
            if max_success_gap_ns >= 1_000_000_000:
                emit("coverage_error", max_success_gap_ms=max_success_gap_ns / 1e6,
                     reason="successful-dump coverage reached the existing one-second interval")
            window_complete = (monitoring_finished >= end and
                               (self.termination_monotonic_ns is None or
                                self.termination_monotonic_ns >= end))
            if not window_complete:
                emit("incomplete", reason="monitoring stopped before requested duration",
                     requested_duration_seconds=self.args.duration,
                     elapsed_seconds=(monitoring_finished - ready) / 1e9,
                     termination_signal=self.termination_signal)
        finally:
            if monitoring_finished is None:
                monitoring_finished = time.monotonic_ns()
            self.stop.set()
            thread.join(1.0)
            def distribution(values):
                ordered = sorted(values)
                return {f"p{p}_ms": ordered[min(len(ordered) - 1, int((len(ordered) - 1) * p / 100))] / 1e6
                        for p in (50, 95, 99)} if ordered else {}
            emit("summary", samples=count, absence_observed=self.absence,
                 target_withdrawals=self.target_withdrawals,
                 max_success_gap_ms=max_success_gap_ns / 1e6,
                 observer_loss=self.loss, query_errors=self.query_errors,
                 window_complete=window_complete,
                 requested_duration_seconds=self.args.duration,
                 elapsed_seconds=(monitoring_finished - ready) / 1e9 if ready is not None else 0,
                 ready_monotonic_ns=ready,
                 monitoring_finished_monotonic_ns=monitoring_finished,
                 termination_signal=self.termination_signal,
                 termination_monotonic_ns=self.termination_monotonic_ns,
                 missed_poll_deadlines=missed_deadlines,
                 cadence_degraded=missed_deadlines > 0,
                 terminal_missed_poll_slots=terminal_missed_slots,
                 window_end_monotonic_ns=end,
                 last_sample_started_monotonic_ns=last_start,
                 last_sample_completed_monotonic_ns=last_complete,
                 interval_min_ms=interval_min / 1e6 if interval_min is not None else None,
                 interval_mean_ms=interval_total / (count - 1) / 1e6 if count > 1 else None,
                 interval_max_ms=interval_max / 1e6,
                 interval_percentiles_last_n=len(intervals),
                 interval_percentiles=distribution(intervals),
                 query_duration_percentiles=distribution(durations))
            self.events.close()
            self.query.close()
        return 1 if (self.absence or self.loss or self.query_errors or self.target_withdrawals
                     or max_success_gap_ns >= 1_000_000_000 or not window_complete) else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("prefixes", nargs="*", default=[f"2001:db{i}::/64" for i in range(4)])
    parser.add_argument("--duration", type=float, default=180)
    parser.add_argument("--poll-ms", type=float, default=10)
    parser.add_argument("--table", type=int, default=254)
    parser.add_argument("--protocol", type=int, help="optional protocol filter; BGP is 186")
    args = parser.parse_args()
    if not 0 < args.duration <= 3600 or not 1 <= args.poll_ms <= 1000:
        parser.error("duration must be (0,3600] seconds and poll-ms must be [1,1000]")
    args.prefixes = list(dict.fromkeys(str(ipaddress.ip_network(p)) for p in args.prefixes))
    versions = {ipaddress.ip_network(p).version for p in args.prefixes}
    if len(versions) != 1:
        parser.error("all prefixes must have the same address family")
    args.family = socket.AF_INET6 if versions == {6} else socket.AF_INET
    try:
        watch = Watch(args)
        def terminate(sig, _):
            watch.termination_signal = sig
            watch.termination_monotonic_ns = time.monotonic_ns()
            watch.stop.set()
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, terminate)
        return watch.run()
    except (OSError, RuntimeError, ValueError, TimeoutError, struct.error) as exc:
        emit("fatal", error=str(exc), errno=getattr(exc, "errno", None))
        return 2


if __name__ == "__main__":
    sys.exit(main())
