<?php
$i = apcu_cache_info(true);
echo $i['num_entries'];
