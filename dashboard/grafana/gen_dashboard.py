#!/usr/bin/env python3
"""Generates the ETF-Pair boards into dashboards/staging/ and the nginx
redirect rules next to this file. The preview Grafana reads the staging
directory, so it shows a change within seconds; the shared Grafana shows
it once deploy/install.sh has copied the boards."""
import os

import boardkit
import combo_spread_board
import live_monitor_board

if __name__ == "__main__":
    print("wrote", boardkit.write("combo-spread", combo_spread_board.build()))
    print("wrote", boardkit.write("live-monitor", live_monitor_board.build()))
    conf = os.path.join(boardkit.HERE, "nginx-board-paths.conf")
    with open(conf, "w") as f:
        f.write(boardkit.board_paths_conf())
    print("wrote", conf)
