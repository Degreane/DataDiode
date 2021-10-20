# -------------------------------------------------------------- #
# functions.bash                                                 #
# Author: Faysal AL-Banna                                        #
# E-Mail: degreane@gmail.com                                     #
# -------------------------------------------------------------- #


## getFileExtension <<<
## input : filename.ext
## return : Extension
## >>>
getFileExtension(){
    fName=$(${BASENAME} $1)
    fExt=$(echo ${fName//[0-9a-z_\ ]*\./} | ${TR} '[:upper:]' '[:lower:]')

    #`echo ${fName//[0-9a-z_\ ]*\./} | ${TR} '[:upper:]' '[:lower:]'`
    if [[ -z ${fExt} ]] ; then
        echo false
        return 0
    fi

    for ext in ${DIODE_FILE_TYPES[@]} ; do
        # echo "${ext} => ${fExt}"
        if [[ $fExt = ${ext} ]]; then
            echo ${ext}
            return 1
        fi
    done
    echo false
    return 0
}

Compressed() {
    ## get file name to be processed
    fName=${1}
    level=$(expr ${2})
    if [[ $level -gt $COMPRESSION_RECURSIVE ]]; then
        echo false
        return 0
    fi
    tmpDir=$(mktemp -d)
    $DIODE_COMPRESS_TOOL extract --outdir "${tmpDir}" "${fName}"
    cwd=$(pwd)
    cd ${tmpDir}
    ls
}