#!/bin/sh
Xvfb "$DISPLAY" -screen 0 1280x800x24 -nolisten tcp &
sleep 1
exec sleep infinity
