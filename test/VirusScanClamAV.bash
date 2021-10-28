#!/bin/bash
CLAMAV=$(which clamscan)

for dirs in /var/www/html/nextcloud/data/*/files ; do ${CLAMAV} -r -i --remove ${dirs} ; done 