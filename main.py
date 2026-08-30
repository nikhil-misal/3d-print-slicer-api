from fastapi import FastAPI, UploadFile, File, Form, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from stl import mesh
import tempfile
import os
import math


app = FastAPI(
    title="3D Print STL Analyzer API",
    version="2.0.0"
)


# ==============================
# CORS
# ==============================

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


# ==============================
# MATERIAL DENSITIES
# Unit: g/cm³
# ==============================

MATERIAL_DENSITIES = {
    "PLA": 1.24,
    "PLA+": 1.25,
    "PETG": 1.27,
    "ABS": 1.04,
    "TPU": 1.21,
    "NYLON": 1.15,
}


# ==============================
# HOME
# ==============================

@app.get("/")
def home():
    return {
        "success": True,
        "status": "online",
        "message": "3D Print STL Analyzer API is running",
        "standard_unit": "mm"
    }


# ==============================
# HEALTH CHECK
# ==============================

@app.get("/health")
def health():
    return {
        "success": True,
        "status": "healthy",
        "unit": "mm"
    }


# ==============================
# HELPER: VALIDATE NUMBER
# ==============================

def safe_float(value, default=0.0):
    try:
        number = float(value)

        if not math.isfinite(number):
            return default

        return number

    except Exception:
        return default


# ==============================
# MAIN STL ANALYSIS API
# ==============================

@app.post("/analyze")
async def analyze_stl(

    file: UploadFile = File(...),

    material: str = Form("PLA"),

    infill: float = Form(20)

):

    # ==============================
    # VALIDATE FILE
    # ==============================

    if not file.filename:
        raise HTTPException(
            status_code=400,
            detail="No file uploaded"
        )

    if not file.filename.lower().endswith(".stl"):
        raise HTTPException(
            status_code=400,
            detail="Only STL files are supported"
        )


    # ==============================
    # VALIDATE MATERIAL
    # ==============================

    material_key = material.strip().upper()

    if material_key not in MATERIAL_DENSITIES:

        raise HTTPException(
            status_code=400,
            detail={
                "error": "Unsupported material",
                "supported_materials":
                    list(MATERIAL_DENSITIES.keys())
            }
        )


    # ==============================
    # VALIDATE INFILL
    # ==============================

    infill = safe_float(infill, 20)

    if infill < 0 or infill > 100:

        raise HTTPException(
            status_code=400,
            detail="Infill must be between 0 and 100"
        )


    temp_path = None


    try:

        # ==============================
        # SAVE TEMPORARY FILE
        # ==============================

        content = await file.read()

        if not content:
            raise HTTPException(
                status_code=400,
                detail="Uploaded STL file is empty"
            )


        with tempfile.NamedTemporaryFile(
            delete=False,
            suffix=".stl"
        ) as temp_file:

            temp_path = temp_file.name

            temp_file.write(content)


        # ==============================
        # LOAD STL
        # ==============================

        model = mesh.Mesh.from_file(
            temp_path
        )


        # ==============================
        # CHECK MODEL
        # ==============================

        if model.vectors is None:

            raise HTTPException(
                status_code=400,
                detail="Invalid STL geometry"
            )


        if len(model.vectors) == 0:

            raise HTTPException(
                status_code=400,
                detail="STL model contains no triangles"
            )


        # ==========================================
        # IMPORTANT:
        # STANDARD UNIT = MILLIMETERS
        #
        # STL format usually does not store units.
        # For 3D printing, raw STL coordinates are
        # treated as millimeters.
        # ==========================================

        scale_factor_to_mm = 1.0

        raw_unit_assumption = "mm"

        unit_used = "mm"


        # ==============================
        # RAW DIMENSIONS
        # ==============================

        min_values = model.vectors.min(
            axis=(0, 1)
        )

        max_values = model.vectors.max(
            axis=(0, 1)
        )


        raw_dimensions = (
            max_values - min_values
        )


        raw_x = safe_float(
            raw_dimensions[0]
        )

        raw_y = safe_float(
            raw_dimensions[1]
        )

        raw_z = safe_float(
            raw_dimensions[2]
        )


        # ==============================
        # STANDARDIZED DIMENSIONS
        # EVERYTHING IN MM
        # ==============================

        x_mm = (
            raw_x *
            scale_factor_to_mm
        )

        y_mm = (
            raw_y *
            scale_factor_to_mm
        )

        z_mm = (
            raw_z *
            scale_factor_to_mm
        )


        # ==============================
        # VALIDATE DIMENSIONS
        # ==============================

        if (
            x_mm <= 0 and
            y_mm <= 0 and
            z_mm <= 0
        ):

            raise HTTPException(
                status_code=400,
                detail="Invalid STL dimensions"
            )


        # ==============================
        # CALCULATE VOLUME
        #
        # numpy-stl returns volume based
        # on the STL coordinates.
        #
        # Since we standardize STL input
        # as mm, this is mm³.
        # ==============================

        volume_raw, _, _ = (
            model.get_mass_properties()
        )


        volume_raw = abs(
            safe_float(volume_raw)
        )


        # Convert to standardized mm³
        volume_mm3 = (
            volume_raw *
            (scale_factor_to_mm ** 3)
        )


        # mm³ to cm³
        volume_cm3 = (
            volume_mm3 / 1000.0
        )


        # ==============================
        # FALLBACK VOLUME CHECK
        # ==============================

        if volume_mm3 <= 0:

            raise HTTPException(
                status_code=400,
                detail=(
                    "Volume is zero or could not be calculated. "
                    "The STL may be an open, broken, flat, or invalid model."
                )
            )


        # ==============================
        # MATERIAL DENSITY
        # ==============================

        density = (
            MATERIAL_DENSITIES[
                material_key
            ]
        )


        # ==============================
        # SOLID WEIGHT
        # ==============================

        solid_weight_g = (
            volume_cm3 *
            density
        )


        # ==============================
        # PRINT WEIGHT ESTIMATION
        #
        # Approximation:
        # Shell/base contribution = 25%
        # Infill contribution = up to 70%
        # ==============================

        shell_factor = 0.25

        infill_factor = (
            (infill / 100.0) *
            0.70
        )


        material_usage_factor = (
            shell_factor +
            infill_factor
        )


        material_usage_factor = max(
            0.10,
            min(
                material_usage_factor,
                1.0
            )
        )


        estimated_print_weight_g = (
            solid_weight_g *
            material_usage_factor
        )


        # ==============================
        # VALIDATE WEIGHT
        # ==============================

        if estimated_print_weight_g < 0:

            estimated_print_weight_g = 0


        # ==============================
        # MODEL SIZE
        # ==============================

        max_dimension_mm = max(
            x_mm,
            y_mm,
            z_mm
        )


        # ==============================
        # RESPONSE
        # ==============================

        return {

            "success": True,

            "status": "model_analyzed",

            "file_name":
                file.filename,


            # --------------------------
            # UNIT INFORMATION
            # --------------------------

            "processing_unit":
                "mm",

            "unit_information": {

                "customer_unit_required":
                    False,

                "automatic_customer_unit_selection":
                    False,

                "standard_processing_unit":
                    "mm",

                "stl_unit_assumption":
                    raw_unit_assumption,

                "scale_factor_to_mm":
                    scale_factor_to_mm
            },


            # --------------------------
            # MATERIAL
            # --------------------------

            "material":
                material_key,

            "infill_percent":
                round(infill, 2),

            "density_g_cm3":
                density,


            # --------------------------
            # DIMENSIONS
            # --------------------------

            "raw_stl_dimensions": {

                "x":
                    round(raw_x, 6),

                "y":
                    round(raw_y, 6),

                "z":
                    round(raw_z, 6)
            },


            "dimensions": {

                "x_mm":
                    round(x_mm, 3),

                "y_mm":
                    round(y_mm, 3),

                "z_mm":
                    round(z_mm, 3),

                "max_dimension_mm":
                    round(
                        max_dimension_mm,
                        3
                    )
            },


            # --------------------------
            # VOLUME
            # --------------------------

            "volume": {

                "mm3":
                    round(
                        volume_mm3,
                        3
                    ),

                "cm3":
                    round(
                        volume_cm3,
                        6
                    )
            },


            # --------------------------
            # WEIGHT
            # --------------------------

            "solid_weight_g":
                round(
                    solid_weight_g,
                    3
                ),

            "estimated_print_weight_g":
                round(
                    estimated_print_weight_g,
                    3
                ),


            # --------------------------
            # CALCULATION INFO
            # --------------------------

            "calculation": {

                "shell_factor":
                    shell_factor,

                "infill_factor":
                    round(
                        infill_factor,
                        4
                    ),

                "material_usage_factor":
                    round(
                        material_usage_factor,
                        4
                    )
            },


            # --------------------------
            # NOTES
            # --------------------------

            "note": (
                "STL files do not normally store unit information. "
                "For standard 3D printing processing, raw STL coordinates "
                "are treated as millimeters."
            )
        }


    except HTTPException:
        raise


    except Exception as e:

        raise HTTPException(
            status_code=500,
            detail={
                "error":
                    "Could not process STL file",

                "message":
                    str(e)
            }
        )


    finally:

        if (
            temp_path and
            os.path.exists(temp_path)
        ):

            try:
                os.remove(
                    temp_path
                )

            except Exception:
                pass
