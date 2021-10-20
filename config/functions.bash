# -------------------------------------------------------------- #
# functions.bash                                                 #
# Author: Faysal AL-Banna                                        #
# E-Mail: degreane@gmail.com                                     #
# -------------------------------------------------------------- #

## Declare Args as an associative array
declare -A ARGS

## getFileExtension <<<
## input : filename.ext
## return : Extension
## >>>

getFileExtension(){
    fName=$(${BASENAME} "${1}")
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

getArgs() {
    while [[ ${#} -gt 0 ]] ; do
        arguement=${1}
        shift
        case ${arguement} in
            -[a-zA-Z]* )
                if [[ ${#} -gt 0 ]]; then
                    value=${1}
                    ARGS[${arguement}]=${value}
                    shift
                fi
        esac
    done
#    parseArgs
}
parseArgs() {
    for ArgKey in "${!ARGS[@]}" ; do
        echo "(${ArgKey} ,${ARGS[${ArgKey}]})"
    done
}
isIn() {
    ## is in is a function that takes an array or an associative array
    ## here we pass the array/hash name and so ..
    if [[ ${#} -eq 2 ]]; then
        local -n AAName=${1}
        local varName=${1}
        local AAType=false
        if [[ "${AAName[@]@A}" =~ '-a' ]]; then
            AAType="Array"
            if [[ ${AAName[*]} =~ " ${2} " ]]; then
                echo true
                return 1
            else
                echo false
                return 0
            fi
        elif [[ "${AAName[@]@A}" =~ '-A' ]]; then
            AAType="Hash"
            if [[ -z ${AAName[${2}]} ]]; then
                echo false
                return 0
            else
                echo true
                return 1
            fi

        else
            echo "isIn: Input is neither array nor hash"
            return 0
        fi
    else
        echo "isIn: Input passed must be two variables"
    fi
}