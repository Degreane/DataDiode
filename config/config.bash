# -------------------------------------------------------------- #
# config.bash                                                    #
# Author: Faysal AL-Banna                                        #
# E-Mail: degreane@gmail.com                                     #
# -------------------------------------------------------------- #



## Description ------------------------------------------------ ##
## configuration file for Ole Scanner as part of Data Diode     ##
## ------------------------------------------------------------ ##

## BASENAME
BASENAME=`which basename`
## TR
TR=`which tr`
## AWK
AWK=`which awk`
## DIODE_COMPRESS_TOOL
DIODE_COMPRESS_TOOL=`which patool`




## COMPRESSION_RECURSIVE <<<
## Number of files to be compressed in a single archive 
## applies to zip,tgz,xz,rar
## defaults to 2
## >>>
COMPRESSION_RECURSIVE=2

## DIODE_WORKING_DIRECTORY <<<
## Directory Specified to work with the files to be examined
## >>>
DIODE_WORKING_DIRECTORY="/home/fbanna/projects/MOD/DataDiode/files"

## DIODE_FILE_TYPES <<<
## Allowed File Types Based On Extension
## a file will be allowed only if its extension is allowed in this variable.
## the file will not be allowed if its extension is not defined as well
## >>>
DIODE_FILE_TYPES=("zip" "tgz" "gz" "xz" "rar" "gzip" "doc" "docx" "xls" "xlsx" "txt" "rtf" "pdf" "jpg" "png" "bmp")

## DIODE_VERBOSE <<<
## Verbosity Level 
## - 0 => None (Default)
## - 1 => informative
## - 2 => Warnings
## - 3 => Debug
## - 4 => Errors
## >>>
DIODE_VERBOSE=0

## DIODE_FILE_STEPS <<<
## hash array contains 
## - extension as index
## - function to call  
## >>>
DIODE_FILE_STEPS=( [zip]="Compressed" [tgz]="Compressed" [gz]="Compressed" [gzip]="Compressed")