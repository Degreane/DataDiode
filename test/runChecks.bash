#!/bin/bash
cd /var/www/html/nextcloud/scripts/DataDiode
FilesIn="/var/www/html/nextcloud/data/*/files"
bash test/VirusScanClamAV.bash
for dirs in "${FilesIn}" ; do
    echo ${dirs}
    for file2check in ${dirs}/*.* ; do
        bash test/CheckFilesAndTypes.bash -F "${file2check}"
        returnValue=$?
        if [[ ${returnValue} -eq 0 ]]; then 
            printf "Checked File %s with Return Value %d [OK] \n" "${file2check}" "${returnValue}"
        else
            printf "Checked File %s with Return Value %d [Deleting] \n" "${file2check}" "${returnValue}"
            /bin/rm -f ${file2check}
            SUDO=$(which sudo)
            WWW_User="www-data"
            NextCloud_Dir="/var/www/html/nextcloud/occ"
            SyncOptions="files:scan --all"
            #echo "${SUDO} -u ${WWW_User} php ${NextCloud_Dir} ${SyncOptions}"
            ${SUDO} -u ${WWW_User} php ${NextCloud_Dir} ${SyncOptions}
        fi
    done
done
cd -