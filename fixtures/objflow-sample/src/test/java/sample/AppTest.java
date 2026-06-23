package sample;

import static org.junit.jupiter.api.Assertions.assertEquals;
import org.junit.jupiter.api.Test;

public class AppTest {
    @Test
    void summaryAssemblesReceiptAcrossStages() {
        // amount 50 -> else branch (LabelStage), net 45 -> "$45"; tier "SILVER"; code "SAVE"
        assertEquals("SAVE/$45/SILVER", new App().summary("save", 50));
    }
}
